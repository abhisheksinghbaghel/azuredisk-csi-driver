/*
Copyright 2017 The Kubernetes Authors.

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
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	schedulerapi "k8s.io/kube-scheduler/extender/v1"

	clientSet "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned/typed/azuredisk/v1alpha1"
)

func init() {

	var err error
	azVolumeAttachmentExtensionClient, azDriverNodeExtensionClient, err = getKubernetesClient()

	if err != nil {
		klog.Fatalf("Failed to create kubernetes clinetset %s ...", err)
		os.Exit(1)
	}
}

func filter(schedulerExtenderArgs schedulerapi.ExtenderArgs) (*schedulerapi.ExtenderFilterResult, error) {
	var (
		filteredNodes     []v1.Node
		filteredNodeNames []string
		failedNodes       map[string]string
	)

	// all available cluster nodes
	allNodes := schedulerExtenderArgs.Nodes.Items
	// all nodes that have azDriverNode running
	azDriverNodes, err := azDriverNodeExtensionClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to get the list of azDriverNodes: %v", err)
	}

	// map node name to azDriverNode state
	nodeNameToStatusMap := make(map[string]string)
	for _, azDriverNode := range azDriverNodes.Items {
		nodeNameToStatusMap[azDriverNode.Spec.NodeName] = azDriverNode.Status.State
	}

	// Filter the nodes based on AzDiverNode status
	failedNodes = make(map[string]string)
	for _, node := range allNodes {
		state, ok := nodeNameToStatusMap[node.Name]
		if ok && state == "Ready" {
			filteredNodes = append(filteredNodes, node)
			filteredNodeNames = append(filteredNodeNames, node.Name)
		} else {
			failedNodes[node.Name] = fmt.Sprintf("AzDriverNodes for %s is not ready.", node.Name)
		}
		klog.V(2).Infof("handleFilterRequest: %v %+v", node.Name, node.Status.Addresses)
	}

	return formatFilterResult(filteredNodes, filteredNodeNames, failedNodes, ""), nil
}

func prioritize(schedulerExtenderArgs schedulerapi.ExtenderArgs) (priorityList schedulerapi.HostPriorityList, err error) {

	availableNodes := schedulerExtenderArgs.Nodes.Items
	requestedVolumes := schedulerExtenderArgs.Pod.Spec.Volumes

	//TODO add RWM volume case here
	// if no volumes are requested, return assigning 0 score to all nodes
	if requestedVolumes == nil {
		for _, node := range availableNodes {
			hostPriority := schedulerapi.HostPriority{Host: node.Name, Score: 0}
			priorityList = append(priorityList, hostPriority)
		}
	} else {
		volumesPodNeeds := make(map[string]bool)
		nodeNameToVolumeMap := make(map[string][]string)
		nodeNameToHeartbeatMap := make(map[string]string)

		// create a lookup map of all the volumes the pod needs
		for _, volume := range requestedVolumes {
			volumesPodNeeds[volume.Name] = true
		}

		// get all nodes that have azDriverNode running
		azDriverNodes, err := azDriverNodeExtensionClient.List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("Failed to get the list of azDriverNodes: %v", err)
		}
		// map azDriverNode name to its heartbeat
		for _, azDriverNode := range azDriverNodes.Items {
			nodeNameToHeartbeatMap[azDriverNode.Spec.NodeName] = azDriverNode.Spec.Heartbeat
		}

		// get all azVolumeAttachments running in the cluster
		azVolumeAttachment, err := azVolumeAttachmentExtensionClient.List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("Failed to get the list of azVolumeAttachments: %v", err)
		}

		// for every volume the pod needs, append its azVolumeAttachment name to the node name
		for _, attachedVolume := range azVolumeAttachment.Items {
			_, needs := volumesPodNeeds[attachedVolume.Spec.AzVolumeName]
			if needs {
				nodeNameToVolumeMap[attachedVolume.Spec.AzDriverNodeName] = append(nodeNameToVolumeMap[attachedVolume.Spec.AzDriverNodeName], attachedVolume.Name)
			}
		}

		// score nodes based in how many azVolumeAttchments are appended to its name
		klog.V(2).Infof("Scoring nodes for pod %+v.", schedulerExtenderArgs.Pod)
		for _, node := range availableNodes {
			score := getNodeScore(len(nodeNameToVolumeMap[node.Name]), nodeNameToHeartbeatMap[node.Name])
			hostPriority := schedulerapi.HostPriority{Host: node.Name, Score: score}
			priorityList = append(priorityList, hostPriority)
			klog.V(2).Infof("Score for %+v is %d.", node, score)
		}
	}
	return
}

func getKubernetesClient() (clientSet.AzVolumeAttachmentInterface, clientSet.AzDriverNodeInterface, error) {

	config, err := rest.InClusterConfig()
	if err != nil {
		// fallback to kubeconfig
		kubeConfigPath := os.Getenv("KUBERNETES_KUBE_CONFIG")
		if strings.EqualFold(kubeConfigPath, "") {
			kubeConfigPath = os.Getenv("HOME") + "/.kube/config"
		}

		// create the config from the path
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfigPath)
		if err != nil {
			return nil, nil, fmt.Errorf("Cannot load kubeconfig: %v", err)
		}
	}

	// generate the clientset extension based off of the config
	azKubeExtensionClient, err := clientSet.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("Cannot create clientset: %v", err)
	}
	azVolumeAttachmentExtensionClient := azKubeExtensionClient.AzVolumeAttachments("")
	azDriverNodeExtensionClient := azKubeExtensionClient.AzDriverNodes("")

	klog.Info("Successfully constructed kubernetes client and extension clientset")
	return azVolumeAttachmentExtensionClient, azDriverNodeExtensionClient, nil
}

func getNodeScore(volumeAttachments int, heartbeat string) int64 {
	// TODO: prioritize disks with low additional volume attachments
	now := time.Now()
	latestHeartbeatWas, err := time.Parse(time.UnixDate, heartbeat)
	if err != nil {
		return 0
	}

	latestHeartbeatCanBe := now.Add(-2 * time.Minute)
	klog.V(2).Infof("Latest node heartbeat was: %s. Latest accepted heartbeat can be: %s", heartbeat, latestHeartbeatCanBe.Format(time.UnixDate))

	if latestHeartbeatWas.Before(latestHeartbeatCanBe) {
		return 0
	}

	score := int64(volumeAttachments*100) - int64(now.Sub(latestHeartbeatWas)/10000000000) //TODO fix logic when all variables are finalized
	return score
}

func formatFilterResult(filteredNodes []v1.Node, nodeNames []string, failedNodes map[string]string, errorMessage string) *schedulerapi.ExtenderFilterResult {
	return &schedulerapi.ExtenderFilterResult{
		Nodes: &v1.NodeList{
			Items: filteredNodes,
		},
		NodeNames:   &nodeNames,
		FailedNodes: failedNodes,
		Error:       errorMessage,
	}
}
