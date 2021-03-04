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
	"math/rand"
	"net/http"
	"net/http/httptest"
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
	handlerFilter     = http.HandlerFunc(handleFilterRequest)
	handlerPrioritize = http.HandlerFunc(handlePrioritizeRequest)
)

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
					getDriverNode("driverNode", ns, "node", "Ready"),
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
						getDriverNode("driverNode", ns, "node", "Ready"),
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
			expectedPrioritizeResult: schedulerapi.HostPriorityList{schedulerapi.HostPriority{Host: "node", Score: getNodeScore(1, time.Now().Format(time.UnixDate))}},
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
						getDriverNode("driverNode", ns, "node", "Pending"),
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
			expectedPrioritizeResult: schedulerapi.HostPriorityList{schedulerapi.HostPriority{Host: "node", Score: getNodeScore(1, time.Now().Format(time.UnixDate))}},
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
						getDriverNode("driverNode", ns, "node", "Ready"),
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
			expectedPrioritizeResult: schedulerapi.HostPriorityList{schedulerapi.HostPriority{Host: "node", Score: getNodeScore(0, time.Now().Format(time.UnixDate))}},
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
						getDriverNode("driverNode0", ns, "node0", "Ready"),
						getDriverNode("driverNode1", ns, "node1", "Ready"),
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
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(1, time.Now().Format(time.UnixDate))},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(0, time.Now().Format(time.UnixDate))},
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
						getDriverNode("driverNode0", ns, "node0", "Pending"),
						getDriverNode("driverNode1", ns, "node1", "Ready"),
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
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(0, time.Now().Format(time.UnixDate))},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(1, time.Now().Format(time.UnixDate))},
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
						getDriverNode("driverNode0", ns, "node0", "Ready"),
						getDriverNode("driverNode1", ns, "node1", "Ready"),
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
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(2, time.Now().Format(time.UnixDate))},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(0, time.Now().Format(time.UnixDate))},
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
						getDriverNode("driverNode0", ns, "node0", "Ready"),
						getDriverNode("driverNode1", ns, "node1", "Ready"),
						getDriverNode("driverNode2", ns, "node2", "Ready"),
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
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(2, time.Now().Format(time.UnixDate))},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(3, time.Now().Format(time.UnixDate))},
				schedulerapi.HostPriority{Host: "node2", Score: getNodeScore(1, time.Now().Format(time.UnixDate))},
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
						getDriverNode("driverNode0", ns, "node0", "Ready"),
						getDriverNode("driverNode1", ns, "node1", "Ready"),
						getDriverNode("driverNode2", ns, "node2", "Ready"),
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
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(2, time.Now().Format(time.UnixDate))},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(3, time.Now().Format(time.UnixDate))},
				schedulerapi.HostPriority{Host: "node2", Score: getNodeScore(0, time.Now().Format(time.UnixDate))},
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
						getDriverNode("driverNode0", ns, "node0", "Ready"),
						getDriverNode("driverNode1", ns, "node1", "Ready"),
						getDriverNode("driverNode2", ns, "node2", "Ready"),
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
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(2, time.Now().Format(time.UnixDate))},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(2, time.Now().Format(time.UnixDate))},
				schedulerapi.HostPriority{Host: "node2", Score: getNodeScore(0, time.Now().Format(time.UnixDate))},
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
	var clusterNodes []v1alpha1Client.AzDriverNode
	var clusterVolumes []v1alpha1Client.AzVolumeAttachment
	var nodes []v1.Node
	var nodeNames []string
	var tokens = make(chan struct{}, 20)
	var wg sync.WaitGroup

	numberOfClusterNodes := 5000
	numberOfClusterVolumes := 30000
	numberOfPodsToSchedule := 100000

	//save original clients
	savedAzVolumeAttachmentExtensionClient := azVolumeAttachmentExtensionClient
	savedAzDriverNodeExtensionClient := azDriverNodeExtensionClient
	defer func() {
		azVolumeAttachmentExtensionClient = savedAzVolumeAttachmentExtensionClient
		azDriverNodeExtensionClient = savedAzDriverNodeExtensionClient
	}()

	// generate large number of nodes
	for i := 0; i < numberOfClusterNodes; i++ {
		nodeName := fmt.Sprintf("node%d", i)
		clusterNodes = append(clusterNodes, getDriverNode(fmt.Sprintf("driverNode%d", i), ns, nodeName, "Ready"))
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
			podVolCount := rand.Intn(numberOfClusterVolumes)
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

	err := <-errorChan
	if err != nil {
		klog.Errorf("Error during stress test: %v ", err)
		t.Fatal(err)
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
		if got[i].Score > want[i].Score {
			return false
		}
	}
	return true
}

func getVolumeAttachment(attachmentName, ns, volumeName, nodeName, state string) v1alpha1Client.AzVolumeAttachment {
	return v1alpha1Client.AzVolumeAttachment{
		TypeMeta: meta.TypeMeta{
			Kind: "azvolumeattachment",
		},
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

func getDriverNode(driverNodeName, ns, nodeName, state string) v1alpha1Client.AzDriverNode {
	return v1alpha1Client.AzDriverNode{
		TypeMeta: meta.TypeMeta{
			Kind: "azdrivernode",
		},
		ObjectMeta: meta.ObjectMeta{
			Name:      driverNodeName,
			Namespace: ns,
		},
		Spec: v1alpha1Client.AzDriverNodeSpec{
			NodeName:  nodeName,
			Partition: 1,
			Heartbeat: time.Now().Format(time.UnixDate),
		},
		Status: v1alpha1Client.AzDriverNodeStatus{
			State: state,
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
