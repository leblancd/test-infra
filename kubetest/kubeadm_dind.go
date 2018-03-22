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

package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	// kubeadm-dind specific flags.
	kubeadmDinDPath = flag.String("kubeadm-dind-path", "",
		"(kubeadm-dind only) Path to the kubeadm-dind-cluster directory. Must be set for kubeadm-dind.")
	kubeadmDinDImage = flag.String("kubeadm-dind-image", "",
		"(kubeadm-dind only) The kubeadm-dind docker image. If unset, it will be built from latest github.com://Mirantis/kubeadm-dind-cluster.")
	kubeadmDinDKubernetesVersion = flag.String("kubeadm-dind-kubernetes-version", "stable",
		"(kubeadm-dind only) Version of Kubernetes to use. May be \"latest\", \"stable\", or a gs:// link to a custom build.")
	kubeadmDinDBuildKubeadm = flag.Bool("kubeadm-dind-build-kubeadm", false,
		"(kubeadm-dind only) Indicates whether kubeadm should be built.")
	kubeadmDinDBuildHyperkube = flag.Bool("kubeadm-dind-build-hyperkube", false,
		"(kubeadm-dind only) Indicates whether hyperkube should be built.")
	kubeadmDinDIPv6Only = flag.Bool("kubeadm-dind-ipv6-only", false,
		"(kubeadm-dind only) Indicates whether cluster should run in IPv6-only mode.")
	kubeadmDinDUpTimeout = flag.Duration("kubeadm-dind-up-timeout", 45*time.Minute,
		"(kubeadm-dind only) Time limit between starting a cluster and making a successful call to the Kubernetes API.")
	kubeadmDinDNumNodes = flag.Int("kubeadm-dind-num-nodes", 4,
		"(kubeadm-dind only) Number of nodes to be deployed in the cluster.")
	kubeadmDinDDumpClusterLogs = flag.Bool("kubeadm-dind-dump-cluster-logs", false,
		"(kubeadm-dind only) Whether to dump cluster logs.")

	// Names that are fixed in the Kubeadm DinD scripts
	gceInstanceName  = "k8s-dind"
	kubeMasterPrefix = "kube-master"
	kubeNodePrefix   = "kube-node"

	// Systemd service logs to collect on the host container
	hostServices = []string{
		"docker",
	}

	// Docker commands to run on Prow and node containers for log dump
	dockerCommands = []struct {
		cmd     string
		logFile string
	}{
		{"docker images", "docker_images.log"},
		{"docker ps -a", "docker_ps.log"},
	}

	// Systemd service logs to collect on the master and worker node containers
	nodeServices = []string{
		"docker",
	}
	//nodeServices = []string{
	//	"kubelet",
	//	"docker",
	//}
	masterKubeLogs = []string{
		"kube-apiserver_*.log",
		"kube-scheduler_*.log",
		"kube-controller-manager_*.log",
		"etcd_*.log",
	}
	nodeKubeLogs = []string{
		"kube-proxy_*.log",
	}

	// Where to look for log files on node containers
	nodeLogDir = "/var/log"

	// Directory for temporarily storing node container log files on
	// the GCE instance
	//instanceTempDir = "/tmp/kubelogs"
)

type kubeadmDinD struct {
	dindPath       string
	dindImage      string
	numNodes       int
	project        string
	zone           string
	buildKubeadm   bool
	buildHyperkube bool
	ipv6Only       bool
	hostCmder      *hostCmder
}

func newKubeadmDinD() (deployer, error) {
	if *kubeadmDinDPath == "" {
		return nil, fmt.Errorf("--kubeadm-dind-path is required")
	}

	k := &kubeadmDinD{
		dindPath:       *kubeadmDinDPath,
		dindImage:      *kubeadmDinDImage,
		numNodes:       *kubeadmDinDNumNodes,
		buildKubeadm:   *kubeadmDinDBuildKubeadm,
		buildHyperkube: *kubeadmDinDBuildHyperkube,
		ipv6Only:       *kubeadmDinDIPv6Only,
	}

	// Set KUBERNETES_CONFORMANCE_TEST so the auth info is picked up
	// from kubectl instead of bash inference.
	if err := os.Setenv("KUBERNETES_CONFORMANCE_TEST", "yes"); err != nil {
		return nil, err
	}

	// Set KUBERNETES_CONFORMANCE_PROVIDER since KUBERNETES_CONFORMANCE_TEST is set
	// to ensure the right provider is passed onto the test.
	if err := os.Setenv("KUBERNETES_CONFORMANCE_PROVIDER", "kubeadm-dind"); err != nil {
		return nil, err
	}

	k.hostCmder = newHostCmder()

	return k, nil
}

func (k *kubeadmDinD) setEnv() error {
	if k.dindImage != "" {
		if err := os.Setenv("DIND_IMAGE", k.dindImage); err != nil {
			return err
		}
	}

	var buildKubeadm, buildHyperkube, ipMode string
	if k.buildKubeadm {
		buildKubeadm = "y"
	}
	if k.buildHyperkube {
		buildHyperkube = "y"
	}
	if k.ipv6Only {
		ipMode = "ipv6"
	}
	envMap := map[string]string{
		"NUM_NODES":       strconv.Itoa(k.numNodes),
		"BUILD_KUBEADM":   buildKubeadm,
		"BUILD_HYPERKUBE": buildHyperkube,
		"IP_MODE":         ipMode,
	}
	for env, val := range envMap {
		if err := os.Setenv(env, val); err != nil {
			return err
		}
	}
	return nil
}

func (k *kubeadmDinD) execCmd(cmd string) *exec.Cmd {
	return k.hostCmder.execCmd(cmd)
}

func (k *kubeadmDinD) runDinDCmd(subCmd string) error {
	dindCmd := filepath.Join(k.dindPath, "dind-cluster.sh")
	cmd := fmt.Sprintf("%s %s", dindCmd, subCmd)
	execCmd := k.execCmd(cmd)
	return control.FinishRunning(execCmd)
}

func (k *kubeadmDinD) buildGinkgo() error {
	// Build e2e.test and ginkgo binaries
	cmd := k.execCmd("make WHAT=test/e2e/e2e.test")
	if err := control.FinishRunning(cmd); err != nil {
		return err
	}
	cmd = k.execCmd("make WHAT=vendor/github.com/onsi/ginkgo/ginkgo")
	return control.FinishRunning(cmd)
}

func (k *kubeadmDinD) Up() error {
	// What we want to emulate:
	//
	// export KUBE_DIND_GCE_PROJECT=dind-v6
	// export KUBE_DIND_GCE_ZONE=us-central1-f
	// export GOOGLE_APPLICATION_CREDENTIALS=$HOME/account.json
	// export BUILD_KUBEADM=y
	// export BUILD_HYPERKUBE=y
	// export IP_MODE=ipv6
	//
	// cd $GOPATH/src/k8s.io/kubernetes
	// source ~/kubeadm-dind-cluster/gce-setup.sh

	k.setEnv()
	cmd := k.execCmd("bash -c pwd")
	cmdOut, err := control.Output(cmd)
	if err != nil {
		return err
	}
	log.Printf("Bringing up DinD cluster, using kubernetes source in '%s'", stripNewline(cmdOut))
	if err := k.runDinDCmd("up"); err != nil {
		return err
	}

	// Build e2e.test and ginkgo binaries
	if err := k.buildGinkgo(); err != nil {
		return err
	}

	if err := k.TestSetup(); err != nil {
		return err
	}

	return waitForReadyNodes(k.numNodes+1, *kubeadmDinDUpTimeout, 1)
}

func (k *kubeadmDinD) IsUp() error {
	return isUp(k)
}

func (k *kubeadmDinD) DumpClusterLogs(localPath, gcsPath string) error {
	if !*kubeadmDinDDumpClusterLogs {
		log.Printf("Cluster log dumping disabled for Kubeadm DinD.")
		return nil
	}

	// ADD CALL TO k.TestSetup()

	// Save logs from host
	if err := k.saveHostLogs(localPath, hostServices); err != nil {
		return err
	}

	// Save logs from  master node containers
	masters, err := k.detectNodeContainers(kubeMasterPrefix)
	if err != nil {
		return err
	}
	for _, master := range masters {
		if err := k.saveNodeLogs(master, localPath, nodeServices, masterKubeLogs); err != nil {
			return err
		}
	}

	// Save logs from  worker node containers
	nodes, err := k.detectNodeContainers(kubeNodePrefix)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if err := k.saveNodeLogs(node, localPath, nodeServices, nodeKubeLogs); err != nil {
			return err
		}
	}

	return nil
}

func (k *kubeadmDinD) TestSetup() error {
	// TODO: Add logic to test for the existence of a kubernetes
	// configuration file.
	fmt.Print("TestSetup method not yet implemented for Kubeadm DinD.")
	return nil
}

func (k *kubeadmDinD) Down() error {
	return k.runDinDCmd("down")
}

func (k *kubeadmDinD) GetClusterCreated(gcpProject string) (time.Time, error) {
	return time.Time{}, errors.New("not implemented")
}

func stripNewline(bytes []byte) string {
	// Strip the newline if one is present
	if bytes[len(bytes)-1] == '\n' {
		bytes = bytes[:len(bytes)-1]
	}
	return string(bytes)
}

type execCmder interface {
	execCmd(cmd string) *exec.Cmd
}

type hostCmder struct{}

func newHostCmder() *hostCmder {
	return new(hostCmder)
}

func (h *hostCmder) execCmd(cmd string) *exec.Cmd {
	// Split string into command and argument strings
	words := strings.Fields(cmd)
	return exec.Command(words[0], words[1:]...)
}

type nodeCmder struct {
	node string
}

func newNodeCmder(node string) *nodeCmder {
	nodeCmder := new(nodeCmder)
	nodeCmder.node = node
	return nodeCmder
}

func (n *nodeCmder) execCmd(cmd string) *exec.Cmd {
	// Docker exec the command in a node container
	return exec.Command("docker", "exec", n.node, cmd)
}

func (k *kubeadmDinD) detectNodeContainers(namePrefix string) ([]string, error) {
	log.Printf("Trying to find node containers with prefix '%s'", namePrefix)
	cmd := k.execCmd("docker ps")
	cmdOut, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var lines []string
	if len(cmdOut) > 0 {
		lines = strings.Split(stripNewline(cmdOut), "\n")
	}
	var nodes []string
	for _, line := range lines {
		if strings.Contains(line, namePrefix) {
			fields := strings.Fields(line)
			if len := len(fields); len != 0 {
				nodes = append(nodes, fields[len-1])
			}
		}
	}
	log.Printf("Found %d", len(nodes))
	return nodes, nil
}

func execCmdSaveLog(cmder execCmder, cmd, logDir, logFile string) error {
	execCmd := cmder.execCmd(cmd)
	cmdOut, err := control.Output(execCmd)
	if err != nil {
		return err
	}
	logPath := filepath.Join(logDir, logFile)
	return ioutil.WriteFile(logPath, cmdOut, 0644)
}

func saveDockerState(cmder execCmder, logDir string) error {
	for _, dockerCommand := range dockerCommands {
		if err := execCmdSaveLog(cmder, dockerCommand.cmd, logDir, dockerCommand.logFile); err != nil {
			return err
		}
	}
	return nil
}

func saveServiceLogs(cmder execCmder, services []string, logDir string) error {
	// Copy service logs
	for _, svc := range services {
		cmd := fmt.Sprintf("journalctl --output=cat -u %s.service", svc)
		logFile := fmt.Sprintf("%s.log", svc)
		if err := execCmdSaveLog(cmder, cmd, logDir, logFile); err != nil {
			return err
		}
	}
	return nil
}

func (k *kubeadmDinD) makeLogDir(logDir string) error {
	cmd := fmt.Sprintf("mkdir -p %s", logDir)
	execCmd := k.execCmd(cmd)
	return control.FinishRunning(execCmd)
}

func (k *kubeadmDinD) saveHostLogs(artifactsDir string, services []string) error {
	log.Printf("Saving logs from host")

	// Create directory for the host container artifacts
	logDir := filepath.Join(artifactsDir, "host-container")
	if err := k.makeLogDir(logDir); err != nil {
		return err
	}

	// Save docker state
	if err := saveDockerState(k.hostCmder, logDir); err != nil {
		return err
	}

	// Copy service logs
	return saveServiceLogs(k.hostCmder, hostServices, logDir)
}

func (k *kubeadmDinD) saveNodeLogs(node string, artifactsDir string, services []string, logFiles []string) error {
	log.Printf("Saving logs from node container %s", node)

	// Create directory for the node container artifacts
	logDir := filepath.Join(artifactsDir, node)
	if err := k.makeLogDir(logDir); err != nil {
		return err
	}

	cmder := newNodeCmder(node)

	// Save docker state
	if err := saveDockerState(cmder, logDir); err != nil {
		return err
	}

	// Copy service logs from the node container
	if err := saveServiceLogs(cmder, services, logDir); err != nil {
		return err
	}

	// Copy log files for kube system pods running in this node container.
	// 'docker cp ...' doesn't support wild carding, so 'find' is used
	// to find logs with names that contain a match string, and then those
	// logs are copied individually.
	for _, logFile := range logFiles {
		log.Printf("Saving %s", logFile)
		cmd := fmt.Sprintf("find %s -name %s -print", nodeLogDir, logFile)
		execCmd := cmder.execCmd(cmd)
		cmdOut, err := control.Output(execCmd)
		if err != nil {
			return err
		}
		var foundFiles []string
		if len(cmdOut) > 0 {
			foundFiles = strings.Split(stripNewline(cmdOut), "\n")
		}
		for _, foundFile := range foundFiles {
			cmd = fmt.Sprintf("docker cp -L %s:%s %s", node, foundFile, logDir)
			execCmd := k.execCmd(cmd)
			if err := control.FinishRunning(execCmd); err != nil {
				return err
			}
		}
	}
	return nil
}
