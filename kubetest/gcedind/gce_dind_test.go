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

package gcedind

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"k8s.io/test-infra/kubetest/process"
)

// fakeExecCmder implements the execCmder interface for testing how the
// deployer processes executed command output.
type fakeExecCmder struct {
	simulatedOutput string // Simulated output
	generateError   bool   // Create a command that causes an error
}

func newFakeExecCmder(simOutput string, genError bool) *fakeExecCmder {
	cmder := new(fakeExecCmder)
	cmder.simulatedOutput = simOutput
	cmder.generateError = genError
	return cmder
}

// execCmd creates an exec.Cmd structure for either:
// - Echoing a simulated output string to be processed by the deployer.
// - Running a bogus command to cause an execution error to be processed
//   by the deployer.
func (f *fakeExecCmder) execCmd(cmd string) *exec.Cmd {
	if f.generateError {
		return exec.Command("Bogus_Command_to_Cause_an_Error")
	}
	return exec.Command("echo", f.simulatedOutput)
}

// fakeNodeCmder implements the nodeCmder interface for testing how the
// deployer processes output from commands executed on a node.
type fakeNodeCmder struct {
	node            string
	simulatedOutput string // Simulated output
	generateError   bool   // Create a command that causes an error
}

func newFakeNodeCmder(node, simOutput string, genError bool) *fakeNodeCmder {
	cmder := new(fakeNodeCmder)
	cmder.node = node
	cmder.simulatedOutput = simOutput
	cmder.generateError = genError
	return cmder
}

// execCmd creates an exec.Cmd structure for either:
// - Echoing a simulated output string to be processed by the deployer.
// - Running a bogus command to cause an execution error to be processed
//   by the deployer.
func (f *fakeNodeCmder) execCmd(cmd string) *exec.Cmd {
	if f.generateError {
		return exec.Command("Bogus_Command_to_Cause_an_Error")
	}
	return exec.Command("echo", f.simulatedOutput)
}

// getNode returns the node name for a fakeNodeCmder
func (f *fakeNodeCmder) getNode() string {
	return f.node
}

// createTestDeployer creates a gce-dind deployer for unit testing.
func createTestDeployer() (*Deployer, error) {
	timeout := time.Duration(10) * time.Second
	interrupt := time.NewTimer(time.Duration(10) * time.Second)
	terminate := time.NewTimer(time.Duration(10) * time.Second)
	verbose := false
	control := process.NewControl(timeout, interrupt, terminate, verbose)
	return NewDeployer("fake-project", "fake-zone", "4", "ipv6", control)
}

// slicesAreEqual tests whether two slices of strings are of equal length
// and have the same entries, independent of ordering. It assumes that
// entries in the slice being compared against (argument 'sliceA', and by
// extension, both slices) form a set.
func slicesAreEqual(sliceA, sliceB []string) bool {
	if len(sliceA) != len(sliceB) {
		return false
	}
	matched := false
	for _, stringA := range sliceA {
		matched = false
		for _, stringB := range sliceB {
			if stringB == stringA {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// TestClusterSize tests whether the clusterSize method:
//     - Processes a sample 'kubectl get nodes --no-header' output and
//       calculates the correct number of nodes, or...
//     - Handles 'kubectl get nodes ...' command errors (reports -1 nodes)
func TestClusterSize(t *testing.T) {
	d, err := createTestDeployer()
	if err != nil {
		t.Errorf("couldn't create deployer: %v", err)
		return
	}

	testCases := []struct {
		testName  string
		simOutput string
		genError  bool
		expSize   int
	}{
		{
			testName:  "No nodes",
			simOutput: "",
			expSize:   0,
		},
		{
			testName: "3-node Cluster",
			simOutput: `
kube-master   Ready     master    10m       v1.11.0
kube-node-1   Ready     <none>    10m       v1.11.0
kube-node-2   Ready     <none>    10m       v1.11.0
`,
			expSize: 3,
		},
		{
			testName: "Simulated command error",
			genError: true,
			expSize:  -1,
		},
	}
	for _, tc := range testCases {
		d.localExecCmder = newFakeExecCmder(tc.simOutput, tc.genError)
		size, err := d.clusterSize()
		if err != nil {
			if !tc.genError {
				t.Errorf("Test case '%s': Unexpected error%v", tc.testName, err)
				continue
			}
		}
		if size != tc.expSize {
			t.Errorf("Test case '%s': expected size %d, found size %d", tc.testName, tc.expSize, size)
			continue
		}
	}
}

// TestDockerMachineConnect tests whether the dockerMachineConnect() method
// correctly performs the equivalent of:
//           eval "$(docker-machine env k8s-dind)"
func TestDockerMachineConnect(t *testing.T) {
	d, err := createTestDeployer()
	if err != nil {
		t.Errorf("couldn't create deployer: %v", err)
		return
	}

	// Simulated output from 'docker-machine env <machine-name>'
	simCmdOutput := "export FOOBAR_TLS_VERIFY=\"1\"\n" +
		"export FOOBAR_HOST=\"tcp://1.2.3.4:2376\"\n" +
		"export FOOBAR_CERT_PATH=\"/workspace\"\n" +
		"export FOOBAR_MACHINE_NAME=\"k8s-dind\"\n" +
		"# Run this command to configure your shell:\n" +
		"# eval $(docker-machine env k8s-dind)\n"

	// Expected environmental variable settings
	expEnv := map[string]string{
		"FOOBAR_TLS_VERIFY":   "1",
		"FOOBAR_HOST":         "tcp://1.2.3.4:2376",
		"FOOBAR_CERT_PATH":    "/workspace",
		"FOOBAR_MACHINE_NAME": "k8s-dind",
	}

	// Test that dockerMachineConnect() sets expected environment variables
	d.localExecCmder = newFakeExecCmder(simCmdOutput, false)
	if err := d.dockerMachineConnect(); err != nil {
		t.Errorf("dockerMachineConnect() error: %v", err)
		return
	}
	for varName, expVal := range expEnv {
		if val := os.Getenv(varName); val != expVal {
			t.Errorf("expected env var '%s=%s', but found '%s=%s'", varName, expVal, varName, val)
			return
		}
	}
}

// TestDetectNodeContainers tests whether detectNodeContainers can
// either correctly process a sample command output for 'kubectl get
// nodes ...', or gracefully handle a command error. Test cases include:
//     - Detect master nodes
//     - Detect worker nodes
//     - Return an empty list upon command error
func TestDetectNodeContainers(t *testing.T) {
	d, err := createTestDeployer()
	if err != nil {
		t.Errorf("couldn't create deployer: %v", err)
		return
	}

	kubectlNodesOutput := `
kube-master   Ready     master    1d        v1.11.0-alpha.0
kube-node-1   Ready     <none>    1d        v1.11.0-alpha.0
kube-node-2   Ready     <none>    1d        v1.11.0-alpha.0
`
	testCases := []struct {
		testName   string
		nodePrefix string
		genError   bool
		expNodes   []string
	}{
		{
			testName:   "Detect master nodes",
			nodePrefix: kubeMasterPrefix,
			expNodes:   []string{"kube-master"},
		},
		{
			testName:   "Detect worker nodes",
			nodePrefix: kubeNodePrefix,
			expNodes:   []string{"kube-node-1", "kube-node-2"},
		},
		{
			testName:   "Check error handling",
			nodePrefix: kubeNodePrefix,
			genError:   true,
			expNodes:   []string{},
		},
	}

	for _, tc := range testCases {
		d.localExecCmder = newFakeExecCmder(kubectlNodesOutput, tc.genError)
		foundNodes, err := d.detectNodeContainers(tc.nodePrefix)
		if err != nil {
			if !tc.genError {
				t.Errorf("Test case: '%s', error: %v", tc.testName, err)
				continue
			}
		}
		// Check whether the expected nodes have all been detected
		if !slicesAreEqual(tc.expNodes, foundNodes) {
			t.Errorf("Test case: '%s', Expected nodes: %v, Detected nodes: %v", tc.testName, tc.expNodes, foundNodes)
			continue
		}
	}
}

// TestDetectKubeContainers tests whether detectKubeContainers can
// either correctly process a sample command output for 'docker ps -a',
// or gracefully handle a command error. Test cases include:
//     - Detect Kubernetes system pod containers on a node
//     - Return an empty list upon command error
func TestDetectKubeContainers(t *testing.T) {
	d, err := createTestDeployer()
	if err != nil {
		t.Errorf("couldn't create deployer: %v", err)
		return
	}

	dockerPsOutput := "CONTAINER ID  IMAGE                             COMMAND                 CREATED       STATUS         PORTS  NAMES\n" +

		"fba3566d4b43  k8s.gcr.io/k8s-dns-sidecar        \"/sidecar --v=2 --log\"  10 minutes ago  Up 10 minutes         k8s_sidecar_kube-dns-69f5bbc4c7\n" +
		"3b7d8cf5b937  k8s.gcr.io/k8s-dns-dnsmasq-nanny  \"/dnsmasq-nanny -v=2 \"  10 minutes ago  Up 10 minutes         k8s_dnsmasq_kube-dns-69f5bbc4c7\n" +
		"5aacb0551aa6  k8s.gcr.io/k8s-dns-kube-dns       \"/kube-dns --domain=c\"  10 minutes ago  Up 10 minutes         k8s_kubedns_kube-dns-69f5bbc4c7\n" +
		"a4abfb755f58  k8s.gcr.io/pause-amd64:3.1        \"/pause\"                10 minutes ago  Up 10 minutes         k8s_POD_kube-dns-69f5bbc4c7\n" +
		"03d1bb19d515  60e55008753b                      \"/usr/local/bin/kube-\"  10 minutes ago  Up 10 minutes         k8s_kube-proxy_kube-proxy-4tzr8\n" +
		"1455bc3829d0  k8s.gcr.io/pause-amd64:3.1        \"/pause\"                10 minutes ago  Up 10 minutes         k8s_POD_kube-proxy-4tzr8\n"

	testCases := []struct {
		testName      string
		genError      bool
		expContainers []string
	}{
		{
			testName: "Detect Containers",
			genError: false,
			expContainers: []string{
				"k8s_sidecar_kube-dns-69f5bbc4c7",
				"k8s_dnsmasq_kube-dns-69f5bbc4c7",
				"k8s_kubedns_kube-dns-69f5bbc4c7",
				"k8s_kube-proxy_kube-proxy-4tzr8",
			},
		},
		{
			testName:      "Check error handling",
			genError:      true,
			expContainers: []string{},
		},
	}

	for _, tc := range testCases {
		fakeCmder := newFakeNodeCmder("fakeNodeName", dockerPsOutput, tc.genError)
		containers, err := d.detectKubeContainers(fakeCmder, nodeKubePods)
		if err != nil {
			if !tc.genError {
				t.Errorf("Test case: '%s', error: %v", tc.testName, err)
				continue
			}
		}
		// Check whether the expected containers have been detected
		if !slicesAreEqual(tc.expContainers, containers) {
			t.Errorf("Test case: '%s', Expected containers: %v, Detected containers: %v", tc.testName, tc.expContainers, containers)
			continue
		}
	}
}
