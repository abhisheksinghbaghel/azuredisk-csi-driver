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
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	schedulerapi "k8s.io/kube-scheduler/extender/v1"
	v1alpha1Client "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/azuredisk/v1alpha1"
	fakeClientSet "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned/fake"
	fakeClientSetInterface "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned/typed/azuredisk/v1alpha1/fake"
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
			inputArgs: "{}",
			want:      http.StatusBadRequest,
		},
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
		//requestArgs := reflect.TypeOf(test.inputArgs).String()
		//save original clients
		savedAzVolumeAttachmentExtensionClient := azVolumeAttachmentExtensionClient
		savedAzDriverNodeExtensionClient := azDriverNodeExtensionClient
		defer func() {
			azVolumeAttachmentExtensionClient = savedAzVolumeAttachmentExtensionClient
			azDriverNodeExtensionClient = savedAzDriverNodeExtensionClient
		}()

		// continue with fake clients
		dummyVolumeAttachment := getVolumeAttachment("volumeAttachment", "vol", "node", "Ready")
		dummyDriverNode := getDriverNode("azDriverNode", "0", "node", "Ready")
		fakeVolumeClient := fakeClientSet.NewSimpleClientset(dummyVolumeAttachment)
		fakeNodeDriverClient := fakeClientSet.NewSimpleClientset(dummyDriverNode)
		azVolumeAttachmentExtensionClient = &fakeClientSetInterface.FakeAzVolumeAttachments{Fake: &fakeClientSetInterface.FakeDiskV1alpha1{&fakeVolumeClient.Fake}}
		azDriverNodeExtensionClient = &fakeClientSetInterface.FakeAzDriverNodes{Fake: &fakeClientSetInterface.FakeDiskV1alpha1{&fakeNodeDriverClient.Fake}}

		response := httptest.NewRecorder()
		requestArgs, err := json.Marshal(test.inputArgs)
		if err != nil {
			t.Fatal("Json encoding failed")
		}

		filterRequest, err := http.NewRequest("GET", filterRequestStr, bytes.NewReader(requestArgs))
		if err != nil {
			t.Fatal(err)
		}

		handlerFilter.ServeHTTP(response, filterRequest)
		if response.Code != test.want {
			t.Errorf("Filter request failed for %s. Got %d want %d", requestArgs, response.Code, test.want)
		}

		prioritizeRequest, err := http.NewRequest("GET", prioritizeRequestStr, bytes.NewReader(requestArgs))
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
		azVolumeAttachment       []runtime.Object
		azDriverNode             []runtime.Object
		schedulerArgs            schedulerapi.ExtenderArgs
		expectedFilterResult     schedulerapi.ExtenderFilterResult
		expectedPrioritizeResult schedulerapi.HostPriorityList
	}{
		{
			name:               "Test simple case of one pod/node/volume",
			azVolumeAttachment: []runtime.Object{getVolumeAttachment("volumeAttachment", "vol", "node", "Ready")},
			azDriverNode:       []runtime.Object{getDriverNode("driverNode", "0", "node", "Ready")},
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
			name:               "Test simple case of pod/node/volume with pending azDriverNode",
			azVolumeAttachment: []runtime.Object{getVolumeAttachment("volumeAttachment", "vol", "node", "Ready")},
			azDriverNode:       []runtime.Object{getDriverNode("driverNode", "0", "node", "Pending")},
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
				FailedNodes: map[string]string{"node": "AzDriverNodes for node is not ready."},
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{schedulerapi.HostPriority{Host: "node", Score: getNodeScore(1, time.Now().Format(time.UnixDate))}},
		},
		{
			name:               "Test simple case of single node/volume with no pod volume requests",
			azVolumeAttachment: []runtime.Object{getVolumeAttachment("volumeAttachment", "vol", "node", "Ready")},
			azDriverNode:       []runtime.Object{getDriverNode("driverNode", "0", "node", "Ready")},
			schedulerArgs: schedulerapi.ExtenderArgs{
				Pod: &v1.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "pod"}},
				Nodes:     &v1.NodeList{Items: []v1.Node{{ObjectMeta: meta.ObjectMeta{Name: "node"}}}},
				NodeNames: &[]string{"node"},
			},
			expectedFilterResult: schedulerapi.ExtenderFilterResult{
				Nodes:       &v1.NodeList{Items: []v1.Node{{ObjectMeta: meta.ObjectMeta{Name: "node"}}}},
				NodeNames:   &[]string{"node"},
				FailedNodes: make(map[string]string),
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{schedulerapi.HostPriority{Host: "node", Score: getNodeScore(0, time.Now().Format(time.UnixDate))}},
		},
		{
			name:               "Test case with 2 nodes and one pod/volume",
			azVolumeAttachment: []runtime.Object{getVolumeAttachment("volumeAttachment", "vol", "node0", "Ready")},
			azDriverNode: []runtime.Object{
				getDriverNode("driverNode0", "0", "node0", "Ready"),
				getDriverNode("driverNode1", "0", "node1", "Ready")},
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
			name:               "Test case with 1 ready and 1 pending nodes and one pod/volume",
			azVolumeAttachment: []runtime.Object{getVolumeAttachment("volumeAttachment", "vol", "node1", "Ready")},
			azDriverNode: []runtime.Object{
				getDriverNode("driverNode0", "0", "node0", "Pending"),
				getDriverNode("driverNode1", "0", "node1", "Ready")},
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
				FailedNodes: map[string]string{"node0": "AzDriverNodes for node0 is not ready."},
				Error:       "",
			},
			expectedPrioritizeResult: schedulerapi.HostPriorityList{
				schedulerapi.HostPriority{Host: "node0", Score: getNodeScore(0, time.Now().Format(time.UnixDate))},
				schedulerapi.HostPriority{Host: "node1", Score: getNodeScore(1, time.Now().Format(time.UnixDate))},
			},
		},
		{
			name: "Test case with 2 nodes/volumes attached to one node",
			azVolumeAttachment: []runtime.Object{
				getVolumeAttachment("volumeAttachment0", "vol", "node0", "Ready"),
				getVolumeAttachment("volumeAttachment1", "vol", "node0", "Ready")},
			azDriverNode: []runtime.Object{
				getDriverNode("driverNode0", "0", "node0", "Ready"),
				getDriverNode("driverNode1", "0", "node1", "Ready")},
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
			azVolumeAttachment: []runtime.Object{
				getVolumeAttachment("volumeAttachment0", "vol", "node2", "Ready"),
				getVolumeAttachment("volumeAttachment1", "vol", "node0", "Ready"),
				getVolumeAttachment("volumeAttachment2", "vol", "node1", "Ready"),
				getVolumeAttachment("volumeAttachment3", "vol", "node1", "Ready"),
				getVolumeAttachment("volumeAttachment4", "vol", "node1", "Ready"),
				getVolumeAttachment("volumeAttachment5", "vol", "node0", "Ready")},
			azDriverNode: []runtime.Object{
				getDriverNode("driverNode0", "0", "node0", "Ready"),
				getDriverNode("driverNode1", "0", "node1", "Ready"),
				getDriverNode("driverNode2", "0", "node2", "Ready")},
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
			azVolumeAttachment: []runtime.Object{
				getVolumeAttachment("volumeAttachment0", "vol2", "node2", "Ready"),
				getVolumeAttachment("volumeAttachment1", "vol", "node0", "Ready"),
				getVolumeAttachment("volumeAttachment2", "vol1", "node1", "Ready"),
				getVolumeAttachment("volumeAttachment3", "vol", "node1", "Ready"),
				getVolumeAttachment("volumeAttachment4", "vol", "node1", "Ready"),
				getVolumeAttachment("volumeAttachment5", "vol", "node0", "Ready")},
			azDriverNode: []runtime.Object{
				getDriverNode("driverNode0", "0", "node0", "Ready"),
				getDriverNode("driverNode1", "0", "node1", "Ready"),
				getDriverNode("driverNode2", "0", "node2", "Ready")},
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
			azVolumeAttachment: []runtime.Object{
				getVolumeAttachment("volumeAttachment0", "vol2", "node2", "Ready"),
				getVolumeAttachment("volumeAttachment1", "vol", "node0", "Ready"),
				getVolumeAttachment("volumeAttachment2", "vol1", "node1", "Ready"),
				getVolumeAttachment("volumeAttachment3", "vol", "node1", "Ready"),
				getVolumeAttachment("volumeAttachment4", "vol", "node1", "Ready"),
				getVolumeAttachment("volumeAttachment5", "vol", "node0", "Ready")},
			azDriverNode: []runtime.Object{
				getDriverNode("driverNode0", "0", "node0", "Ready"),
				getDriverNode("driverNode1", "0", "node1", "Ready"),
				getDriverNode("driverNode2", "0", "node2", "Ready")},
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
			fakeVolumeClient := fakeClientSet.NewSimpleClientset(test.azVolumeAttachment...)
			fakeNodeDriverClient := fakeClientSet.NewSimpleClientset(test.azDriverNode...)
			azVolumeAttachmentExtensionClient = &fakeClientSetInterface.FakeAzVolumeAttachments{Fake: &fakeClientSetInterface.FakeDiskV1alpha1{&fakeVolumeClient.Fake}}
			azDriverNodeExtensionClient = &fakeClientSetInterface.FakeAzDriverNodes{Fake: &fakeClientSetInterface.FakeDiskV1alpha1{&fakeNodeDriverClient.Fake}}

			// encode scheduler arguments
			requestArgs, err := json.Marshal(&test.schedulerArgs)
			if err != nil {
				t.Fatal("Json encoding failed")
			}

			// check filter result
			filterRequest, err := http.NewRequest("GET", filterRequestStr, bytes.NewReader(requestArgs))
			if err != nil {
				t.Fatal(err)
			}

			filterResultRecorder := httptest.NewRecorder()
			handlerFilter.ServeHTTP(filterResultRecorder, filterRequest)

			decoder := json.NewDecoder(filterResultRecorder.Body)
			var actualFilterResult schedulerapi.ExtenderFilterResult
			if err := decoder.Decode(&actualFilterResult); err != nil {
				klog.Errorf("handleFilterRequest: Error decoding filter request: %v", err)
			}

			if !gotExpectedFilterResults(actualFilterResult, test.expectedFilterResult) {
				t.Errorf("Actual filter response (%s) does not equal expected response.", filterResultRecorder.Body)
			}

			// check prioritize result
			prioritizeRequest, err := http.NewRequest("GET", prioritizeRequestStr, bytes.NewReader(requestArgs))
			if err != nil {
				t.Fatal(err)
			}

			prioritizeResultRecorder := httptest.NewRecorder()
			handlerPrioritize.ServeHTTP(prioritizeResultRecorder, prioritizeRequest)

			decoder = json.NewDecoder(prioritizeResultRecorder.Body)
			var actualPrioritizeList schedulerapi.HostPriorityList
			if err := decoder.Decode(&actualPrioritizeList); err != nil {
				klog.Errorf("handlePrioritizeRequest: Error decoding filter request: %v", err)
			}

			if !gotExpectedPrioritizeList(actualPrioritizeList, test.expectedPrioritizeResult) {
				t.Errorf("Actual prioritize response (%s) does not equal expected response.", prioritizeResultRecorder.Body /*, args, expectedFilterResponse*/)
			}
		})
	}
}

//TODO add testcase with randomized input

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

func getVolumeAttachment(attachmentName, volumeName, nodeName, state string) *v1alpha1Client.AzVolumeAttachment {
	return &v1alpha1Client.AzVolumeAttachment{
		ObjectMeta: meta.ObjectMeta{
			Name: attachmentName,
		},
		Spec: v1alpha1Client.AzVolumeAttachmentSpec{
			AzVolumeName:     volumeName,
			AzDriverNodeName: nodeName,
			Partition:        1,
		},
		Status: v1alpha1Client.AzVolumeAttachmentStatus{
			State: state,
		},
	}
}

func getDriverNode(driverNodeName, csiNodeID, nodeName, state string) *v1alpha1Client.AzDriverNode {
	return &v1alpha1Client.AzDriverNode{
		ObjectMeta: meta.ObjectMeta{
			Name: driverNodeName,
		},
		Spec: v1alpha1Client.AzDriverNodeSpec{
			CSINodeID: csiNodeID,
			NodeName:  nodeName,
			Partition: 1,
			Heartbeat: time.Now().Format(time.UnixDate),
		},
		Status: v1alpha1Client.AzDriverNodeStatus{
			State: state,
		},
	}

}
