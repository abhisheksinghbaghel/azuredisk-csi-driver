/*
Copyright 2019 The Kubernetes Authors.

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
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	schedulerapi "k8s.io/kube-scheduler/extender/v1"
	v1alpha1Client "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/azuredisk/v1alpha1"
	fakeClientSet "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned/fake"
)

var (
	handlerFilter          = http.HandlerFunc(handleFilterRequest)
	handlerPrioritize      = http.HandlerFunc(handlePrioritizeRequest)
	KubeConfigFileEnvVar   = "KUBECONFIG"
	validKubeConfigPath    = "valid-Kube-Config-Path"
	validKubeConfigContent = `
	apiVersion: v1
    clusters:
    - cluster:
        server: https://foo-cluster-dns-57e0bda1.hcp.westus2.azmk8s.io:443
      name: foo-cluster
    contexts:
    - context:
        cluster: foo-cluster
        user: clusterUser_foo-rg_foo-cluster
      name: foo-cluster
    current-context: foo-cluster
    kind: Config
    preferences: {}
    users:
    - name: clusterUser_foo-rg_foo-cluster
      user:
`
)

func TestMain(m *testing.M) {
	existingConfigPath, _ := createConfigFileAndSetEnv(validKubeConfigPath, validKubeConfigContent, KubeConfigFileEnvVar)

	exitVal := m.Run()
	if len(existingConfigPath) > 0 {
		defer cleanConfigAndRestoreEnv(validKubeConfigPath, KubeConfigFileEnvVar, existingConfigPath)
	}
	os.Exit(exitVal)
}

func TestFilterAndPrioritizeRequestResponseCode(t *testing.T) {
	tests := []struct {
		inputArgs interface{}
		want      int
	}{
		{
			inputArgs: &schedulerapi.ExtenderArgs{
				Pod:       &v1.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod"}},
				Nodes:     &v1.NodeList{Items: []v1.Node{{ObjectMeta: meta.ObjectMeta{Name: "node"}}}},
				NodeNames: &[]string{"node"}},
			want: http.StatusOK,
		},
		{
			inputArgs: &schedulerapi.ExtenderArgs{
				Pod:       &v1.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod"}},
				Nodes:     &v1.NodeList{Items: nil},
				NodeNames: &[]string{"node"}},
			want: http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		//save original clients
		savedAzVolumeAttachmentExtensionClient := azVolumeAttachmentExtensionClient
		savedAzDriverNodeExtensionClient := azDriverNodeExtensionClient
		defer func() {
			azVolumeAttachmentExtensionClient = savedAzVolumeAttachmentExtensionClient
			azDriverNodeExtensionClient = savedAzDriverNodeExtensionClient
		}()

		// continue with fake clients
		testClientSet := fakeClientSet.NewSimpleClientset(
			&v1alpha1Client.AzVolumeAttachmentList{
				Items: []v1alpha1Client.AzVolumeAttachment{
					getVolumeAttachment("volumeAttachment", ns, "vol", "node", "Ready"),
				},
			},
			&v1alpha1Client.AzDriverNodeList{
				Items: []v1alpha1Client.AzDriverNode{
					getDriverNode("driverNode", ns, "node", true),
				},
			},
		)
		azVolumeAttachmentExtensionClient = testClientSet.DiskV1alpha1().AzVolumeAttachments(ns)
		azDriverNodeExtensionClient = testClientSet.DiskV1alpha1().AzDriverNodes(ns)

		response := httptest.NewRecorder()
		requestArgs, err := json.Marshal(test.inputArgs)
		if err != nil {
			t.Fatal("Json encoding failed")
		}

		filterRequest, err := http.NewRequest("POST", filterRequestStr, bytes.NewReader(requestArgs))
		if err != nil {
			t.Fatal(err)
		}

		handlerFilter.ServeHTTP(response, filterRequest)
		if response.Code != test.want {
			t.Errorf("Filter request failed for %s. Got %d want %d", requestArgs, response.Code, test.want)
		}

		prioritizeRequest, err := http.NewRequest("POST", prioritizeRequestStr, bytes.NewReader(requestArgs))
		if err != nil {
			t.Fatal(err)
		}

		handlerPrioritize.ServeHTTP(response, prioritizeRequest)
		if response.Code != test.want {
			t.Errorf("Filter request failed for %s. Got %d want %d", requestArgs, response.Code, test.want)
		}
	}
}

func TestFilterAndPrioritizeResponses(t *testing.T) {
	tests := []struct {
		name                     string
		testClientSet            *fakeClientSet.Clientset
		schedulerArgs            schedulerapi.ExtenderArgs
		expectedFilterResult     schedulerapi.ExtenderFilterResult
		expectedPrioritizeResult schedulerapi.HostPriorityList
	}{
		{
			name: "Test simple case of one pod/node/volume",
			testClientSet: fakeClientSet.NewSimpleClientset(
				&v1alpha1Client.AzVolumeAttachmentList{
					Items: []v1alpha1Client.AzVolumeAttachment{
						getVolumeAttachment("volumeAttachment", ns, "vol", "node", "Ready"),
					},
				},
				&v1alpha1Client.AzDriverNodeList{
					Items: []v1alpha1Client.AzDriverNode{
						getDriverNode("driverNode", ns, "node", true),
					},
				},
			),
			schedulerArgs: schedulerapi.ExtenderArgs{
				Pod: &v1.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "pod"},
					Spec: v1.PodSpec{
						Volumes: []v1.Volume{
							{
								Name: "vol",
							},
						}}},
				Nodes:     &v1.NodeList{Items: []v1.Node{{ObjectMeta: meta.ObjectMeta{Name: "node"}}}},
				NodeNames: &[]string{"node"},
			},
			expectedFilterResult: schedulerapi.ExtenderFilterResult{
				Nodes:       &v1.NodeList{Items: []v1.Node{{ObjectMeta: meta.ObjectMeta{Name: "node"}}}},
				NodeNames:   &[]string{"node"},
				FailedNodes: make(map[string]string),
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{schedulerapi.HostPriority{Host: "node", Score: getNodeScore(1, time.Now().UnixNano())}},
		},
		{
			name: "Test simple case of pod/node/volume with pending azDriverNode",
			testClientSet: fakeClientSet.NewSimpleClientset(
				&v1alpha1Client.AzVolumeAttachmentList{
					Items: []v1alpha1Client.AzVolumeAttachment{
						getVolumeAttachment("volumeAttachment", ns, "vol", "node", "Ready"),
					},
				},
				&v1alpha1Client.AzDriverNodeList{
					Items: []v1alpha1Client.AzDriverNode{
						getDriverNode("driverNode", ns, "node", false),
					},
				},
			),
			schedulerArgs: schedulerapi.ExtenderArgs{
				Pod: &v1.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "pod"},
					Spec: v1.PodSpec{
						Volumes: []v1.Volume{
							{
								Name: "vol",
							},
						}}},
				Nodes:     &v1.NodeList{Items: []v1.Node{{ObjectMeta: meta.ObjectMeta{Name: "node"}}}},
				NodeNames: &[]string{"node"},
			},
			expectedFilterResult: schedulerapi.ExtenderFilterResult{
				Nodes:       &v1.NodeList{Items: nil},
				NodeNames:   nil,
				FailedNodes: map[string]string{"node": "AzDriverNode for node is not ready."},
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{schedulerapi.HostPriority{Host: "node", Score: getNodeScore(1, time.Now().UnixNano())}},
		},
		{
			name: "Test simple case of single node/volume with no pod volume requests",
			testClientSet: fakeClientSet.NewSimpleClientset(
				&v1alpha1Client.AzVolumeAttachmentList{
					Items: []v1alpha1Client.AzVolumeAttachment{
						getVolumeAttachment("volumeAttachment", ns, "vol", "node", "Ready"),
					},
				},
				&v1alpha1Client.AzDriverNodeList{
					Items: []v1alpha1Client.AzDriverNode{
						getDriverNode("driverNode", ns, "node", true),
					},
				},
			),
			schedulerArgs: schedulerapi.ExtenderArgs{
				Pod: &v1.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "pod"}},
				Nodes:     &v1.NodeList{Items: []v1.Node{{ObjectMeta: meta.ObjectMeta{Name: "node"}}}},
				NodeNames: &[]string{"node"},
			},
			expectedFilterResult: schedulerapi.ExtenderFilterResult{
				Nodes:       &v1.NodeList{},
				NodeNames:   nil,
				FailedNodes: nil,
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{schedulerapi.HostPriority{Host: "node", Score: getNodeScore(0, time.Now().UnixNano())}},
		},
		{
			name: "Test case with 2 nodes and one pod/volume",
			testClientSet: fakeClientSet.NewSimpleClientset(
				&v1alpha1Client.AzVolumeAttachmentList{
					Items: []v1alpha1Client.AzVolumeAttachment{
						getVolumeAttachment("volumeAttachment", ns, "vol", "node0", "Ready"),
					},
				},
				&v1alpha1Client.AzDriverNodeList{
					Items: []v1alpha1Client.AzDriverNode{
						getDriverNode("driverNode0", ns, "node0", true),
						getDriverNode("driverNode1", ns, "node1", true),
					},
				},
			),
			schedulerArgs: schedulerapi.ExtenderArgs{
				Pod: &v1.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "pod"},
					Spec: v1.PodSpec{
						Volumes: []v1.Volume{
							{
								Name: "vol",
							},
						},
					},
				},
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node0"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
					},
				},
				NodeNames: &[]string{"node0, node1"},
			},
			expectedFilterResult: schedulerapi.ExtenderFilterResult{
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node0"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
					},
				},
				NodeNames:   &[]string{"node0", "node1"},
				FailedNodes: make(map[string]string),
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(1, time.Now().UnixNano())},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(0, time.Now().UnixNano())},
			},
		},
		{
			name: "Test case with 1 ready and 1 pending nodes and one pod/volume",
			testClientSet: fakeClientSet.NewSimpleClientset(
				&v1alpha1Client.AzVolumeAttachmentList{
					Items: []v1alpha1Client.AzVolumeAttachment{
						getVolumeAttachment("volumeAttachment", ns, "vol", "node1", "Ready"),
					},
				},
				&v1alpha1Client.AzDriverNodeList{
					Items: []v1alpha1Client.AzDriverNode{
						getDriverNode("driverNode0", ns, "node0", false),
						getDriverNode("driverNode1", ns, "node1", true),
					},
				},
			),
			schedulerArgs: schedulerapi.ExtenderArgs{
				Pod: &v1.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "pod"},
					Spec: v1.PodSpec{
						Volumes: []v1.Volume{
							{
								Name: "vol",
							},
						},
					},
				},
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node0"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
					},
				},
				NodeNames: &[]string{"node0, node1"},
			},
			expectedFilterResult: schedulerapi.ExtenderFilterResult{
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
					},
				},
				NodeNames:   &[]string{"node1"},
				FailedNodes: map[string]string{"node0": "AzDriverNode for node0 is not ready."},
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(0, time.Now().UnixNano())},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(1, time.Now().UnixNano())},
			},
		},
		{
			name: "Test case with 2 nodes/volumes attached to one node",
			testClientSet: fakeClientSet.NewSimpleClientset(
				&v1alpha1Client.AzVolumeAttachmentList{
					Items: []v1alpha1Client.AzVolumeAttachment{
						getVolumeAttachment("volumeAttachment0", ns, "vol", "node0", "Ready"),
						getVolumeAttachment("volumeAttachment1", ns, "vol", "node0", "Ready"),
					},
				},
				&v1alpha1Client.AzDriverNodeList{
					Items: []v1alpha1Client.AzDriverNode{
						getDriverNode("driverNode0", ns, "node0", true),
						getDriverNode("driverNode1", ns, "node1", true),
					},
				},
			),
			schedulerArgs: schedulerapi.ExtenderArgs{
				Pod: &v1.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "pod"},
					Spec: v1.PodSpec{
						Volumes: []v1.Volume{
							{
								Name: "vol",
							},
						},
					},
				},
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node0"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
					},
				},
				NodeNames: &[]string{"node0, node1"},
			},
			expectedFilterResult: schedulerapi.ExtenderFilterResult{
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node0"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
					},
				},
				NodeNames:   &[]string{"node0", "node1"},
				FailedNodes: make(map[string]string),
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(2, time.Now().UnixNano())},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(0, time.Now().UnixNano())},
			},
		},
		{
			name: "Test case with 3 nodes and 6 volumes attached to multiple nodes",
			testClientSet: fakeClientSet.NewSimpleClientset(
				&v1alpha1Client.AzVolumeAttachmentList{
					Items: []v1alpha1Client.AzVolumeAttachment{
						getVolumeAttachment("volumeAttachment0", ns, "vol", "node2", "Ready"),
						getVolumeAttachment("volumeAttachment1", ns, "vol", "node0", "Ready"),
						getVolumeAttachment("volumeAttachment2", ns, "vol", "node1", "Ready"),
						getVolumeAttachment("volumeAttachment3", ns, "vol", "node1", "Ready"),
						getVolumeAttachment("volumeAttachment4", ns, "vol", "node1", "Ready"),
						getVolumeAttachment("volumeAttachment5", ns, "vol", "node0", "Ready"),
					},
				},
				&v1alpha1Client.AzDriverNodeList{
					Items: []v1alpha1Client.AzDriverNode{
						getDriverNode("driverNode0", ns, "node0", true),
						getDriverNode("driverNode1", ns, "node1", true),
						getDriverNode("driverNode2", ns, "node2", true),
					},
				},
			),
			schedulerArgs: schedulerapi.ExtenderArgs{
				Pod: &v1.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "pod"},
					Spec: v1.PodSpec{
						Volumes: []v1.Volume{
							{
								Name: "vol",
							}}}},
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node0"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node2"}}}},
				NodeNames: &[]string{"node0", "node1", "node2"},
			},
			expectedFilterResult: schedulerapi.ExtenderFilterResult{
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node0"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node2"}}}},
				NodeNames:   &[]string{"node0", "node1", "node2"},
				FailedNodes: make(map[string]string),
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(2, time.Now().UnixNano())},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(3, time.Now().UnixNano())},
				schedulerapi.HostPriority{Host: "node2", Score: getNodeScore(1, time.Now().UnixNano())},
			},
		},
		{
			name: "Test case with 3 nodes, extra volumes and pod with 2 volume requests",
			testClientSet: fakeClientSet.NewSimpleClientset(
				&v1alpha1Client.AzVolumeAttachmentList{
					Items: []v1alpha1Client.AzVolumeAttachment{
						getVolumeAttachment("volumeAttachment0", ns, "vol2", "node2", "Ready"),
						getVolumeAttachment("volumeAttachment1", ns, "vol", "node0", "Ready"),
						getVolumeAttachment("volumeAttachment2", ns, "vol1", "node1", "Ready"),
						getVolumeAttachment("volumeAttachment3", ns, "vol", "node1", "Ready"),
						getVolumeAttachment("volumeAttachment4", ns, "vol", "node1", "Ready"),
						getVolumeAttachment("volumeAttachment5", ns, "vol", "node0", "Ready"),
					},
				},
				&v1alpha1Client.AzDriverNodeList{
					Items: []v1alpha1Client.AzDriverNode{
						getDriverNode("driverNode0", ns, "node0", true),
						getDriverNode("driverNode1", ns, "node1", true),
						getDriverNode("driverNode2", ns, "node2", true),
					},
				},
			),
			schedulerArgs: schedulerapi.ExtenderArgs{
				Pod: &v1.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "pod"},
					Spec: v1.PodSpec{
						Volumes: []v1.Volume{
							{
								Name: "vol",
							},
							{
								Name: "vol1",
							}}}},
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node0"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node2"}}}},
				NodeNames: &[]string{"node0", "node1", "node2"},
			},
			expectedFilterResult: schedulerapi.ExtenderFilterResult{
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node0"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node2"}}}},
				NodeNames:   &[]string{"node0", "node1", "node2"},
				FailedNodes: make(map[string]string),
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(2, time.Now().UnixNano())},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(3, time.Now().UnixNano())},
				schedulerapi.HostPriority{Host: "node2", Score: getNodeScore(0, time.Now().UnixNano())},
			},
		},
		{
			name: "Test case with 3 nodes and extra volumes attached to multiple nodes",
			testClientSet: fakeClientSet.NewSimpleClientset(
				&v1alpha1Client.AzVolumeAttachmentList{
					Items: []v1alpha1Client.AzVolumeAttachment{
						getVolumeAttachment("volumeAttachment0", ns, "vol2", "node2", "Ready"),
						getVolumeAttachment("volumeAttachment1", ns, "vol", "node0", "Ready"),
						getVolumeAttachment("volumeAttachment2", ns, "vol1", "node1", "Ready"),
						getVolumeAttachment("volumeAttachment3", ns, "vol", "node1", "Ready"),
						getVolumeAttachment("volumeAttachment4", ns, "vol", "node1", "Ready"),
						getVolumeAttachment("volumeAttachment5", ns, "vol", "node0", "Ready"),
					},
				},
				&v1alpha1Client.AzDriverNodeList{
					Items: []v1alpha1Client.AzDriverNode{
						getDriverNode("driverNode0", ns, "node0", true),
						getDriverNode("driverNode1", ns, "node1", true),
						getDriverNode("driverNode2", ns, "node2", true),
					},
				},
			),
			schedulerArgs: schedulerapi.ExtenderArgs{
				Pod: &v1.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "pod"},
					Spec: v1.PodSpec{
						Volumes: []v1.Volume{
							{
								Name: "vol",
							}}}},
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node0"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node2"}}}},
				NodeNames: &[]string{"node0", "node1", "node2"},
			},
			expectedFilterResult: schedulerapi.ExtenderFilterResult{
				Nodes: &v1.NodeList{
					Items: []v1.Node{
						{ObjectMeta: meta.ObjectMeta{Name: "node0"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node1"}},
						{ObjectMeta: meta.ObjectMeta{Name: "node2"}}}},
				NodeNames:   &[]string{"node0", "node1", "node2"},
				FailedNodes: make(map[string]string),
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(2, time.Now().UnixNano())},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(2, time.Now().UnixNano())},
				schedulerapi.HostPriority{Host: "node2", Score: getNodeScore(0, time.Now().UnixNano())},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			//save original clients
			savedAzVolumeAttachmentExtensionClient := azVolumeAttachmentExtensionClient
			savedAzDriverNodeExtensionClient := azDriverNodeExtensionClient
			defer func() {
				azVolumeAttachmentExtensionClient = savedAzVolumeAttachmentExtensionClient
				azDriverNodeExtensionClient = savedAzDriverNodeExtensionClient
			}()

			// continue with fake clients
			azVolumeAttachmentExtensionClient = test.testClientSet.DiskV1alpha1().AzVolumeAttachments(ns)
			azDriverNodeExtensionClient = test.testClientSet.DiskV1alpha1().AzDriverNodes(ns)

			// encode scheduler arguments
			requestArgs, err := json.Marshal(&test.schedulerArgs)
			if err != nil {
				t.Fatal("Json encoding failed")
			}

			// check filter result
			filterRequest, err := http.NewRequest("POST", filterRequestStr, bytes.NewReader(requestArgs))
			if err != nil {
				t.Fatal(err)
			}

			filterResultRecorder := httptest.NewRecorder()
			handlerFilter.ServeHTTP(filterResultRecorder, filterRequest)

			decoder := json.NewDecoder(filterResultRecorder.Body)
			var actualFilterResult schedulerapi.ExtenderFilterResult
			if err := decoder.Decode(&actualFilterResult); err != nil {
				klog.Errorf("handleFilterRequest: Error decoding filter request: %v", err)
				t.Fatal(err)
			}

			if !gotExpectedFilterResults(actualFilterResult, test.expectedFilterResult) {
				t.Errorf("Actual filter response (%s) does not equal expected response.", filterResultRecorder.Body)
			}

			// check prioritize result
			prioritizeRequest, err := http.NewRequest("POST", prioritizeRequestStr, bytes.NewReader(requestArgs))
			if err != nil {
				t.Fatal(err)
			}

			prioritizeResultRecorder := httptest.NewRecorder()
			handlerPrioritize.ServeHTTP(prioritizeResultRecorder, prioritizeRequest)

			decoder = json.NewDecoder(prioritizeResultRecorder.Body)
			var actualPrioritizeList schedulerapi.HostPriorityList
			if err := decoder.Decode(&actualPrioritizeList); err != nil {
				klog.Errorf("handlePrioritizeRequest: Error decoding filter request: %v", err)
				t.Fatal(err)
			}

			if !gotExpectedPrioritizeList(actualPrioritizeList, test.expectedPrioritizeResult) {
				t.Errorf("Actual prioritize response (%s) does not equal expected response.", prioritizeResultRecorder.Body)
			}
		})
	}
}

//TODO test only checks the repsonse code. add check for response body
func TestFilterAndPrioritizeInRandomizedLargeCluster(t *testing.T) {
	var nodes []v1.Node
	var nodeNames []string

	//TODO increase numberOfPodsToSchedule when changing the implemention to reuse goroutines
	stressTestSetupParams := map[string][3]int{
		"low":  [3]int{500, 5000, 1000},
		"avg":  [3]int{1000, 10000, 1000},
		"high": [3]int{5000, 30000, 1000},
	}

	//save original clients
	savedAzVolumeAttachmentExtensionClient := azVolumeAttachmentExtensionClient
	savedAzDriverNodeExtensionClient := azDriverNodeExtensionClient
	defer func() {
		azVolumeAttachmentExtensionClient = savedAzVolumeAttachmentExtensionClient
		azDriverNodeExtensionClient = savedAzDriverNodeExtensionClient
	}()

	for _, setupParams := range stressTestSetupParams {
		var tokens = make(chan struct{}, 20)
		var wg sync.WaitGroup
		var clusterNodes []v1alpha1Client.AzDriverNode
		var clusterVolumes []v1alpha1Client.AzVolumeAttachment
		numberOfClusterNodes := setupParams[0]
		numberOfClusterVolumes := setupParams[1]
		numberOfPodsToSchedule := setupParams[2]

		// generate large number of nodes
		for i := 0; i < numberOfClusterNodes; i++ {
			nodeName := fmt.Sprintf("node%d", i)
			clusterNodes = append(clusterNodes, getDriverNode(fmt.Sprintf("driverNode%d", i), ns, nodeName, true))
			nodes = append(nodes, v1.Node{ObjectMeta: meta.ObjectMeta{Name: nodeName}})
			nodeNames = append(nodeNames, nodeName)
		}

		// generate volumes and assign to nodes
		for i := 0; i < numberOfClusterVolumes; i++ {
			clusterVolumes = append(clusterVolumes, getVolumeAttachment(fmt.Sprintf("volumeAttachment%d", i), ns, fmt.Sprintf("vol%d", i), fmt.Sprintf("node%d", rand.Intn(5000)), "Ready"))
		}

		testClientSet := fakeClientSet.NewSimpleClientset(
			&v1alpha1Client.AzVolumeAttachmentList{
				Items: clusterVolumes,
			},
			&v1alpha1Client.AzDriverNodeList{
				Items: clusterNodes,
			})

		// continue with fake clients
		azVolumeAttachmentExtensionClient = testClientSet.DiskV1alpha1().AzVolumeAttachments(ns)
		azDriverNodeExtensionClient = testClientSet.DiskV1alpha1().AzDriverNodes(ns)

		var errorChan = make(chan error, numberOfPodsToSchedule)
		for j := 0; j < numberOfPodsToSchedule; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var testPodVolumes []v1.Volume
				// randomly assign volumes to pod
				podVolCount := rand.Intn(256)
				for i := 0; i < podVolCount; i++ {
					testPodVolumes = append(testPodVolumes, v1.Volume{Name: fmt.Sprintf("vol%d", rand.Intn(numberOfClusterVolumes))})
				}

				testPod := &v1.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "pod"},
					Spec: v1.PodSpec{
						Volumes: testPodVolumes}}

				schedulerArgs := schedulerapi.ExtenderArgs{
					Pod:       testPod,
					Nodes:     &v1.NodeList{Items: nodes},
					NodeNames: &nodeNames,
				}

				responseFilter := httptest.NewRecorder()
				responsePrioritize := httptest.NewRecorder()
				requestArgs, err := json.Marshal(schedulerArgs)
				if err != nil {
					errorChan <- fmt.Errorf("Json encoding failed")
					return
				}

				tokens <- struct{}{} // acquire a token
				filterRequest, err := http.NewRequest("POST", filterRequestStr, bytes.NewReader(requestArgs))
				if err != nil {
					errorChan <- err
					return
				}

				handlerFilter.ServeHTTP(responseFilter, filterRequest)
				if responseFilter.Code != 200 {
					errorChan <- fmt.Errorf("Filter request failed for %s. Got %d want %d", requestArgs, responseFilter.Code, 200)
					return
				}

				prioritizeRequest, err := http.NewRequest("POST", prioritizeRequestStr, bytes.NewReader(requestArgs))
				if err != nil {
					errorChan <- err
					return
				}

				handlerPrioritize.ServeHTTP(responsePrioritize, prioritizeRequest)
				if responsePrioritize.Code != 200 {
					errorChan <- fmt.Errorf("Filter request failed for %s. Got %d want %d", requestArgs, responsePrioritize.Code, 200)
					return
				}

				decoder := json.NewDecoder(responseFilter.Body)
				var filterResult schedulerapi.ExtenderFilterResult
				if err := decoder.Decode(&filterResult); err != nil {
					errorChan <- fmt.Errorf("handleFilterRequest: Error decoding filter request: %v", err)
					return
				}

				decoder = json.NewDecoder(responsePrioritize.Body)
				var prioritizeList schedulerapi.HostPriorityList
				if err := decoder.Decode(&prioritizeList); err != nil {
					errorChan <- fmt.Errorf("handlePrioritizeRequest: Error decoding filter request: %v", err)
					return
				}
				errorChan <- nil
				<-tokens //release the token
			}()
		}

		go func() {
			wg.Wait()
			close(errorChan)
			close(tokens)
		}()

		j := 0
		for err := range errorChan {
			if err != nil {
				klog.Errorf("Error during stress test: %v ", err)
				t.Fatal(err)
			}
			j++
			if j > numberOfPodsToSchedule { // TODO remove. Helps with debugging otherwise unnecessary
				klog.Info("Test ran successfully.")
				break
			}
		}
	}
}

func gotExpectedFilterResults(got, want schedulerapi.ExtenderFilterResult) bool {
	return reflect.DeepEqual(got, want)
}

func gotExpectedPrioritizeList(got, want schedulerapi.HostPriorityList) bool {
	for i := range want {
		if got[i].Host != want[i].Host {
			return false
		}
		// if got[i].Score > want[i].Score { //TODO add logic to validate score
		// 	return false
		// }
	}
	return true
}

func getVolumeAttachment(attachmentName, ns, volumeName, nodeName, state string) v1alpha1Client.AzVolumeAttachment {
	return v1alpha1Client.AzVolumeAttachment{
		ObjectMeta: meta.ObjectMeta{
			Name:      attachmentName,
			Namespace: ns,
		},
		Spec: v1alpha1Client.AzVolumeAttachmentSpec{
			UnderlyingVolume: volumeName,
			AzDriverNodeName: nodeName,
			Partition:        1,
		},
		Status: v1alpha1Client.AzVolumeAttachmentStatus{
			State: state,
		},
	}
}

func getDriverNode(driverNodeName, ns, nodeName string, ready bool) v1alpha1Client.AzDriverNode {
	heartbeat := time.Now().UnixNano()
	return v1alpha1Client.AzDriverNode{
		ObjectMeta: meta.ObjectMeta{
			Name:      driverNodeName,
			Namespace: ns,
		},
		Spec: v1alpha1Client.AzDriverNodeSpec{
			NodeName: nodeName,
		},
		Status: &v1alpha1Client.AzDriverNodeStatus{
			ReadyForVolumeAllocation: &ready,
			LastHeartbeatTime:        &heartbeat,
		},
	}

}

// TODO add back with integration tests
// func getPod(podName, ns, containerName, containerImage string) *v1.Pod {
// 	return &v1.Pod{
// 		ObjectMeta: meta.ObjectMeta{
// 			Name:      podName,
// 			Namespace: ns,
// 		},
// 		Spec: v1.PodSpec{
// 			SchedulerName: "azdiskschedulerextender",
// 			Containers: []v1.Container{
// 				v1.Container{
// 					Name:  containerName,
// 					Image: containerImage,
// 				},
// 			},
// 		},
// 	}
// }

func createConfigFileAndSetEnv(path string, content string, envVariableName string) (string, error) {
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if err != nil {
		return "", err
	}

	if err := ioutil.WriteFile(path, []byte(content), 0666); err != nil {
		return "", err
	}

	envValue, _ := os.LookupEnv(envVariableName)
	err = os.Setenv(envVariableName, path)
	if err != nil {
		return "", fmt.Errorf("Failed to set env variable")
	}

	return envValue, err
}

func cleanConfigAndRestoreEnv(path string, envVariableName string, envValue string) {
	defer os.Setenv(envVariableName, envValue)
	os.Remove(path)
}
