/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This file implements a Kubernetes kubetest deployer based on the scripts
// in the Mirantis/kubeadm-dind-cluster github repo. These scripts are used
// to create a containerized, Docker-in-Docker (DinD) based Kubernetes cluster
// that is running in a remote GCE instance.

package gcedind

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/test-infra/kubetest/process"
)

var (
	// Names that are fixed in the Kubeadm DinD scripts
	gceInstanceName  = "k8s-dind"
	kubeMasterPrefix = "kube-master"
	kubeNodePrefix   = "kube-node"

	// Logs to collect on the GCE instance for log dump
	serialLogFile    = "serial-1.log"
	kernelLogFile    = "kernel.log"
	instanceServices = []string{
		"docker",
	}

	// Docker commands to run on GCE instance and nodes for log dump
	dockerCommands = []struct {
		cmd     string
		logFile string
	}{
		{"docker images", "docker_images.log"},
		{"docker ps -a", "docker_ps.log"},
	}

	// Logs to collect on the master and worker node containers for log dump
	systemdServices = []string{
		"kubelet",
		"docker",
	}
	masterKubePods = []string{
		"kube-apiserver",
		"kube-scheduler",
		"kube-controller-manager",
		"kube-proxy",
		"etcd",
	}
	nodeKubePods = []string{
		"kube-proxy",
		"kube-dns",
	}

	// Where to look for (nested) container log files on the node containers
	nodeLogDir = "/var/log"

	// Path to Kubernetes source tree
	kubernetesDir = "/workspace/k8s.io/kubernetes"

	// Path to kubeadm-dind-cluster scripts
	dindScriptsPath = "/workspace/github.com/leblancd/kubeadm-dind-cluster/fixed"

	// Location of container image for kubeadm-dind-cluster (K-D-C) scripts.
	// TODO: This URL can be deleted once the Mirantis K-D-C stable branch
	// is modified to pull its own pre-built image. This change will be made
	// either by adding a new GCE setup script for the stable K-D-C branch,
	// or by https://github.com/Mirantis/kubeadm-dind-cluster/pull/123.
	dindImage = "mirantis/kubeadm-dind-cluster:stable"

	// Maximum number of nodes to create
	maxNumNodes = 10
)

// Deployer is used to implement a kubetest deployer interface
type Deployer struct {
	numNodes       int
	project        string
	zone           string
	ipv6Only       bool
	localExecCmder execCmder
	control        *process.Control
}

// NewDeployer returns a new GCE-DinD Deployer
func NewDeployer(project, zone, numNodes, ipMode string, control *process.Control) (*Deployer, error) {
	if project == "" {
		return nil, fmt.Errorf("--deployment=gce-dind requires --gcp-project")
	}

	if zone == "" {
		zone = "us-central1-c"
	}

	nodes, err := strconv.Atoi(numNodes)
	if err != nil {
		return nil, fmt.Errorf("Unexpected config: --num-nodes=%s, error: %v", numNodes, err)
	}
	if nodes > maxNumNodes {
		return nil, fmt.Errorf("Configured --num-nodes=%d exceeds maximum of %d", nodes, maxNumNodes)
	}
	if ipMode == "dual-stack" {
		return nil, fmt.Errorf("Configured --ip-mode=%s is not supported for --deployment=gce-dind", ipMode)
	}
	ipv6Only := ipMode == "ipv6"

	d := &Deployer{
		numNodes:       nodes,
		project:        project,
		zone:           zone,
		ipv6Only:       ipv6Only,
		localExecCmder: new(localExecCmder),
		control:        control,
	}

	return d, nil
}

// Up brings up a containerized Kubernetes cluster on a GCE instance.
func (d *Deployer) Up() error {

	d.setClusterEnv()

	// Scripts must be run from Kubernetes source directory
	if err := os.Chdir(kubernetesDir); err != nil {
		return fmt.Errorf("Could not change directory to %s: '%v'", kubernetesDir, err)
	}

	// Bring up a cluster on a GCE instance
	gceSetupScript := filepath.Join(dindScriptsPath, "dind-cluster-stable.sh")
	if err := d.run(gceSetupScript + " up"); err != nil {
		return err
	}

	// Build e2e.test and ginkgo binaries
	if err := d.run("make WHAT=test/e2e/e2e.test"); err != nil {
		return err
	}
	return d.run("make WHAT=vendor/github.com/onsi/ginkgo/ginkgo")
}

// IsUp determines if a cluster is up based on whether one or more nodes
// is ready.
func (d *Deployer) IsUp() error {
	n, err := d.clusterSize()
	if err != nil {
		return err
	}
	if n <= 0 {
		return fmt.Errorf("cluster found, but %d nodes reported", n)
	}
	return nil
}

// DumpClusterLogs copies dumps docker state and service logs for:
// - GCE instance
// - Kube master node containers
// - Kube worker node containers
// to a local artifacts directory.
func (d *Deployer) DumpClusterLogs(localPath, gcsPath string) error {
	// Save logs from GCE instance
	if err := d.saveInstanceLogs(localPath); err != nil {
		return err
	}

	// Save logs from  master node container(s)
	if err := d.saveMasterNodeLogs(localPath); err != nil {
		return err
	}

	// Save logs from  worker node containers
	return d.saveWorkerNodeLogs(localPath)
}

// TestSetup is a no-op for this deployer.
func (d *Deployer) TestSetup() error {
	return nil
}

// Down brings the DinD-based cluster down and cleans up any DinD state
func (d *Deployer) Down() error {
	// Bring the cluster down and clean up DinD state
	//dindScript := fmt.Sprintf(filepath.Join(dindScriptsPath, "dind-cluster-stable.sh"))
	//clusterDownCommands := []string{
	//	dindScript + " down",
	//	dindScript + " clean",
	//}
	//for _, cmd := range clusterDownCommands {
	//	if err := d.run(cmd); err != nil {
	//		return err
	//	}
	//}
	return nil
}

// GetClusterCreated is not yet implemented.
func (d *Deployer) GetClusterCreated(gcpProject string) (time.Time, error) {
	return time.Time{}, errors.New("not implemented")
}

// execCmder defines an interface for providing a wrapper for processing
// command line strings before calling os/exec.Command().
// There are three implementations of this interface defined below:
// - localExecCmder:    For executing commands locally (e.g. in Prow container).
// - instanceExecCmder: For executing commands on the GCE instance.
// - nodeExecCmder:     For executing commands on node containers on the GCE
//                      instance.
type execCmder interface {
	execCmd(cmd string) *exec.Cmd
}

// localExecCmder implements the execCmder interface for processing commands
// locally.
type localExecCmder struct{}

// execCmd splits a command line string into a command (first word) and
// remaining arguments in variadic form, as required by exec.Command().
func (l *localExecCmder) execCmd(cmd string) *exec.Cmd {
	words := strings.Fields(cmd)
	return exec.Command(words[0], words[1:]...)
}

// instanceExecCmder implements the execCmder interface for processing
// commands on the GCE instance.
type instanceExecCmder struct {
	project string
	zone    string
}

func (d *Deployer) newInstanceExecCmder() *instanceExecCmder {
	cmder := new(instanceExecCmder)
	cmder.project = d.project
	cmder.zone = d.zone
	return cmder
}

// execCmd creates an exec.Cmd structure for running a command line on
// the GCE instance. It is equivalent to remotely running the command via
// 'gcloud compute ssh ... --command <cmd>'.
func (i *instanceExecCmder) execCmd(cmd string) *exec.Cmd {
	// Command will be executed via gcloud SSH
	return exec.Command("gcloud", "compute", "ssh", "--ssh-flag", "-o LogLevel=quiet", "--ssh-flag", "-o ConnectTimeout=30", "--project", i.project, "--zone", i.zone, gceInstanceName, "--command", cmd)
}

type nodeCmder interface {
	execCmd(cmd string) *exec.Cmd
	getNode() string
}

// nodeExecCmder implements the execCmder interface and the
// nodeCmder interface for processing commands
// on a node container that is running on the GCE instance.
type nodeExecCmder struct {
	node string
}

func newNodeExecCmder(node string) *nodeExecCmder {
	cmder := new(nodeExecCmder)
	cmder.node = node
	return cmder
}

// execCmd creates an exec.Cmd structure for running a command line on
// a node container on the GCE instance. It is equivalent to running
// the command via 'docker exec <node-container-name> <cmd>'.
func (n *nodeExecCmder) execCmd(cmd string) *exec.Cmd {
	args := strings.Fields(fmt.Sprintf("exec %s %s", n.node, cmd))
	return exec.Command("docker", args...)
}

// getNode returns the node name for a nodeExecCmder
func (n *nodeExecCmder) getNode() string {
	return n.node
}

// runBashCmd invokes a command line via a bash shell, equivalent to
// 'bash -c <cmd>'.
func (d *Deployer) runBashCmd(cmd string) error {
	execCmd := exec.Command("bash", "-c", cmd)
	err := d.control.FinishRunning(execCmd)
	if err != nil {
		fmt.Printf("Error: '%v'", err)
	}
	return err
}

// run runs a command locally and prints any errors.
func (d *Deployer) run(cmd string) error {
	execCmd := d.localExecCmder.execCmd(cmd)
	err := d.control.FinishRunning(execCmd)
	if err != nil {
		fmt.Printf("Error: '%v'", err)
	}
	return err
}

// getOutput runs a command locally, prints any errors, and returns command output.
func (d *Deployer) getOutput(cmd string) ([]byte, error) {
	execCmd := d.localExecCmder.execCmd(cmd)
	o, err := d.control.Output(execCmd)
	if err != nil {
		log.Printf("Error: '%v'", err)
		return nil, err
	}
	return o, nil
}

// outputWithStderr runs a command and returns combined stdout and stderr.
func (d *Deployer) outputWithStderr(cmd *exec.Cmd) ([]byte, error) {
	var stdOutErr bytes.Buffer
	cmd.Stdout = &stdOutErr
	cmd.Stderr = &stdOutErr
	err := d.control.FinishRunning(cmd)
	return stdOutErr.Bytes(), err
}

// execCmdSaveLog executes a command either locally, on the GCE instance,
// or on a node container, and writes the combined stdout and stderr to a
// log file in a local artifacts directory. (Stderr is required because
// running 'docker logs ...' on nodes sometimes returns results as stderr).
func (d *Deployer) execCmdSaveLog(cmder execCmder, cmd string, logDir string, logFile string) error {
	log.Printf("Saving %s", logFile)
	execCmd := cmder.execCmd(cmd)
	o, err := d.outputWithStderr(execCmd)
	if err != nil {
		log.Printf("Error: '%v'", err)
		return err
	}
	logPath := filepath.Join(logDir, logFile)
	return ioutil.WriteFile(logPath, o, 0644)
}

// setClusterEnv sets environment variables for building and testing
// a cluster.
func (d *Deployer) setClusterEnv() error {
	var ipMode string
	if d.ipv6Only {
		ipMode = "ipv6"
	}
	// Set KUBERNETES_CONFORMANCE_TEST so that the master IP address
	// is derived from kube config rather than through gcloud.
	envMap := map[string]string{
		"NUM_NODES":                   strconv.Itoa(d.numNodes),
		"KUBE_DIND_GCE_PROJECT":       d.project,
		"KUBE_DIND_GCE_ZONE":          d.zone,
		"DIND_IMAGE":                  dindImage,
		"BUILD_KUBEADM":               "y",
		"BUILD_HYPERKUBE":             "y",
		"IP_MODE":                     ipMode,
		"KUBERNETES_CONFORMANCE_TEST": "yes",
		"REMOTE_DNS64_V4SERVER":       "173.37.87.157",
	}
	for env, val := range envMap {
		if err := os.Setenv(env, val); err != nil {
			return err
		}
	}
	return nil
}

// dockerMachineConnect sets a docker-machine connection from the local
// host to the GCE instance. It is equivalent to setting environment
// variables via 'eval $(docker-machine env k8s-dind)'.
func (d *Deployer) dockerMachineConnect() error {
	o, err := d.getOutput("docker-machine env k8s-dind")
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(string(o))
	if trimmed != "" {
		lines := strings.Split(trimmed, "\n")
		for _, line := range lines {
			// If the output line is of the form:
			//     export KEY="VALUE"
			// then trim the leading "export ", remove the quotes, and split
			// into KEY and VALUE fields
			if strings.Contains(line, "export") {
				keyPair := strings.Split(strings.Replace(strings.TrimPrefix(line, "export "), "\"", "", -1), "=")
				os.Setenv(keyPair[0], keyPair[1])
			}
		}
	}
	return nil
}

// clusterSize determines the number of nodes in a cluster.
func (d *Deployer) clusterSize() (int, error) {
	o, err := d.getOutput("kubectl get nodes --no-headers")
	if err != nil {
		return -1, fmt.Errorf("kubectl get nodes failed: %s\n%s", err, string(o))
	}
	trimmed := strings.TrimSpace(string(o))
	if trimmed != "" {
		return len(strings.Split(trimmed, "\n")), nil
	}
	return 0, nil
}

// saveInstanceLogs collects a serial log, service logs, and docker state
// from the GCE instance, and saves the logs in a local artifacts directory.
func (d *Deployer) saveInstanceLogs(artifactsDir string) error {
	log.Printf("Saving logs from GCE instance")

	// Create directory for GCE instance artifacts
	logDir := filepath.Join(artifactsDir, "gce-instance")
	if err := d.run("mkdir -p " + logDir); err != nil {
		return err
	}

	// Copy the serial log for the instance
	log.Printf("Saving serial log")
	cmd := fmt.Sprintf("gcloud compute instances get-serial-port-output --project %s --zone %s --port 1 %s", d.project, d.zone, gceInstanceName)
	if err := d.execCmdSaveLog(d.localExecCmder, cmd, logDir, serialLogFile); err != nil {
		return err
	}

	// Save docker state
	for _, dockerCommand := range dockerCommands {
		if err := d.execCmdSaveLog(d.localExecCmder, dockerCommand.cmd, logDir, dockerCommand.logFile); err != nil {
			return err
		}
	}

	// Copy kernel and service logs from the instance
	instanceExecCmder := d.newInstanceExecCmder()
	if err := d.execCmdSaveLog(instanceExecCmder, "journalctl --output=short-precise -k", logDir, kernelLogFile); err != nil {
		return err
	}
	for _, svc := range instanceServices {
		cmd := fmt.Sprintf("journalctl --output=cat -u %s.service", svc)
		logFile := fmt.Sprintf("%s.log", svc)
		if err := d.execCmdSaveLog(instanceExecCmder, cmd, logDir, logFile); err != nil {
			return err
		}
	}
	return nil
}

// saveMasterNodeLogs collects docker state, service logs, and Kubernetes
// system pod logs from all master node containers on the GCE instance,
// and saves the logs in a local artifacts directory.
func (d *Deployer) saveMasterNodeLogs(artifactsDir string) error {
	masters, err := d.detectNodeContainers(kubeMasterPrefix)
	if err != nil {
		return err
	}
	for _, master := range masters {
		if err := d.saveNodeLogs(master, artifactsDir, systemdServices, masterKubePods); err != nil {
			return err
		}
	}
	return nil
}

// saveWorkerNodeLogs collects docker state, service logs, and Kubernetes
// system pod logs from all worker node containers on the GCE instance,
// and saves the logs in a local artifacts directory.
func (d *Deployer) saveWorkerNodeLogs(artifactsDir string) error {
	nodes, err := d.detectNodeContainers(kubeNodePrefix)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if err := d.saveNodeLogs(node, artifactsDir, systemdServices, nodeKubePods); err != nil {
			return err
		}
	}
	return nil
}

// detectNodeContainers creates a list of names for either all master or all
// worker node containers. It does this by running 'kubectl get nodes ... '
// and searching for container names that begin with a specified name prefix.
func (d *Deployer) detectNodeContainers(namePrefix string) ([]string, error) {
	log.Printf("Looking for container names beginning with '%s'", namePrefix)
	o, err := d.getOutput("kubectl get nodes --no-headers")
	if err != nil {
		return nil, err
	}
	var nodes []string
	trimmed := strings.TrimSpace(string(o))
	if trimmed != "" {
		lines := strings.Split(trimmed, "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			name := fields[0]
			if strings.Contains(name, namePrefix) {
				nodes = append(nodes, name)
			}
		}
	}
	return nodes, nil
}

// detectKubeContainers creates a list of containers (either running or
// exited) on a master or worker node whose names contain any of a list of
// Kubernetes system pod name substings.
func (d *Deployer) detectKubeContainers(nodeCmder nodeCmder, kubePods []string) ([]string, error) {
	// Run 'docker ps -a' on the node container
	cmd := fmt.Sprintf("docker ps -a")
	execCmd := nodeCmder.execCmd(cmd)
	o, err := d.control.Output(execCmd)
	if err != nil {
		log.Printf("Error running '%s' on %s: '%v'", cmd, nodeCmder.getNode(), err)
		return nil, err
	}
	// Find container names that contain any of a list of pod name substrings
	var containers []string
	if trimmed := strings.TrimSpace(string(o)); trimmed != "" {
		lines := strings.Split(trimmed, "\n")
		for _, line := range lines {
			if fields := strings.Fields(line); len(fields) > 0 {
				name := fields[len(fields)-1]
				if strings.Contains(name, "_POD_") {
					// Ignore infra containers
					continue
				}
				for _, pod := range kubePods {
					if strings.Contains(name, pod) {
						containers = append(containers, name)
						break
					}
				}
			}
		}
	}
	return containers, nil
}

// saveNodeLogs collects docker state, service logs, and Kubernetes
// system pod logs for a given node container, and saves the logs in a local
// artifacts directory.
func (d *Deployer) saveNodeLogs(node string, artifactsDir string, services []string, kubePods []string) error {
	log.Printf("Saving logs from node container %s", node)

	// Create directory for node container artifacts
	logDir := filepath.Join(artifactsDir, node)
	if err := d.run("mkdir -p " + logDir); err != nil {
		return err
	}

	nodeExecCmder := newNodeExecCmder(node)

	// Save docker state for this node
	for _, dockerCommand := range dockerCommands {
		if err := d.execCmdSaveLog(nodeExecCmder, dockerCommand.cmd, logDir, dockerCommand.logFile); err != nil {
			return err
		}
	}

	// Copy service logs from the node container
	for _, svc := range services {
		cmd := fmt.Sprintf("journalctl --output=cat -u %s.service", svc)
		logFile := fmt.Sprintf("%s.log", svc)
		if err := d.execCmdSaveLog(nodeExecCmder, cmd, logDir, logFile); err != nil {
			return err
		}
	}

	// Copy log files for kube system pod containers (running or exited)
	// from this node container.
	containers, err := d.detectKubeContainers(nodeExecCmder, kubePods)
	if err != nil {
		return err
	}
	for _, container := range containers {
		cmd := fmt.Sprintf("docker logs %s", container)
		logFile := fmt.Sprintf("%s.log", container)
		if err := d.execCmdSaveLog(nodeExecCmder, cmd, logDir, logFile); err != nil {
			return err
		}
	}
	return nil
}
