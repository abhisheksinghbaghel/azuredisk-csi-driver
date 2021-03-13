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
	"path/filepath"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	schedulerapi "k8s.io/kube-scheduler/extender/v1"

	v1alpha1Meta "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/azuredisk/v1alpha1"
	clientSet "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned/typed/azuredisk/v1alpha1"
)

var (
	azVolumeAttachmentExtensionClient clientSet.AzVolumeAttachmentInterface
	azDriverNodeExtensionClient       clientSet.AzDriverNodeInterface
	kubeExtensionClientset            *clientSet.DiskV1alpha1Client
	ns                                = "default" //TODO update to correct namespace when finalized
)

type azDriverNodesMeta struct {
	nodes *v1alpha1Meta.AzDriverNodeList
	err   error
}

type azVolumeAttachmentsMeta struct {
	volumes *v1alpha1Meta.AzVolumeAttachmentList
	err     error
}

func initSchedulerExtender() {
	var err error
	kubeExtensionClientset, err = getKubernetesExtensionClientsets()
	if err != nil {
		klog.Fatalf("Failed to create kubernetes clientset %s ...", err)
		os.Exit(1)
	}

	azVolumeAttachmentExtensionClient, azDriverNodeExtensionClient = kubeExtensionClientset.AzVolumeAttachments(ns), kubeExtensionClientset.AzDriverNodes(ns)
}

func filter(context context.Context, schedulerExtenderArgs schedulerapi.ExtenderArgs) (*schedulerapi.ExtenderFilterResult, error) {
	var (
		filteredNodes     []v1.Node
		filteredNodeNames []string
		failedNodes       map[string]string
	)

	requestedVolumes := schedulerExtenderArgs.Pod.Spec.Volumes

	//TODO add RWM volume case here
	// if no volumes are requested, return assigning 0 score to all nodes
	if len(requestedVolumes) != 0 {
		nodesChan := make(chan azDriverNodesMeta)
		// all available cluster nodes
		allNodes := schedulerExtenderArgs.Nodes.Items

		go getAzDriverNodes(context, nodesChan)
		azDriverNodesMeta := <-nodesChan
		if azDriverNodesMeta.err != nil {
			filteredNodes = schedulerExtenderArgs.Nodes.Items
			filteredNodeNames = *schedulerExtenderArgs.NodeNames
			klog.V(2).Infof("Failed to get the list of azDriverNodes: %v", azDriverNodesMeta.err)

			return formatFilterResult(filteredNodes, filteredNodeNames, failedNodes, ""), nil
		}

		// map node name to azDriverNode state
		nodeNameToStatusMap := make(map[string]string)
		for _, azDriverNode := range azDriverNodesMeta.nodes.Items {
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
				failedNodes[node.Name] = fmt.Sprintf("AzDriverNode for %s is not ready.", node.Name)
			}
			klog.V(2).Infof("handleFilterRequest: %v %+v", node.Name, node.Status.Addresses)
		}
	}
	return formatFilterResult(filteredNodes, filteredNodeNames, failedNodes, ""), nil
}

func prioritize(context context.Context, schedulerExtenderArgs schedulerapi.ExtenderArgs) (priorityList schedulerapi.HostPriorityList, err error) {

	availableNodes := schedulerExtenderArgs.Nodes.Items
	requestedVolumes := schedulerExtenderArgs.Pod.Spec.Volumes

	//TODO add RWM volume case here
	// if no volumes are requested, return assigning 0 score to all nodes
	if len(requestedVolumes) == 0 {
		priorityList = setNodeSocresToZero(availableNodes)
	} else {
		volumesPodNeeds := make(map[string]struct{})
		nodeNameToVolumeMap, nodeNameToHeartbeatMap := make(map[string][]string), make(map[string]string)
		nodesChan, volumesChan := make(chan azDriverNodesMeta), make(chan azVolumeAttachmentsMeta)

		go getAzDriverNodes(context, nodesChan)
		go getAzVolumeAttachments(context, volumesChan)

		// create a lookup map of all the volumes the pod needs
		for _, volume := range requestedVolumes {
			volumesPodNeeds[volume.Name] = struct{}{}
		}

		// get all nodes that have azDriverNode running
		azDriverNodesMeta := <-nodesChan
		if azDriverNodesMeta.err != nil {
			priorityList = setNodeSocresToZero(availableNodes)
			klog.V(2).Infof("Failed to get the list of azDriverNodes: %v", azDriverNodesMeta.err)
			return
		}

		// map azDriverNode name to its heartbeat
		for _, azDriverNode := range azDriverNodesMeta.nodes.Items {
			nodeNameToHeartbeatMap[azDriverNode.Spec.NodeName] = azDriverNode.Spec.Heartbeat
		}

		// get all azVolumeAttachments running in the cluster
		azVolumeAttachmentsMeta := <-volumesChan
		if azVolumeAttachmentsMeta.err != nil {
			priorityList = setNodeSocresToZero(availableNodes)
			klog.V(2).Infof("Failed to get the list of azVolumeAttachments: %v", azVolumeAttachmentsMeta.err)
			return
		}

		// for every volume the pod needs, append its azVolumeAttachment name to the node name
		for _, attachedVolume := range azVolumeAttachmentsMeta.volumes.Items {
			_, needs := volumesPodNeeds[attachedVolume.Spec.UnderlyingVolume]
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

func getKubeConfig() (config *rest.Config, err error) {
	config, err = rest.InClusterConfig()
	if err != nil {
		klog.Warning("Failed getting the in cluster config: %v", err)
		// fallback to kubeconfig
		kubeConfigPath := os.Getenv("KUBECONFIG")
		if len(kubeConfigPath) == 0 {
			kubeConfigPath = filepath.Join(os.Getenv("HOME"), ".kube", "config")
		}

		// create the config from the path
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfigPath)
	}
	return
}

func getKubernetesExtensionClientsets() (azKubeExtensionClientset *clientSet.DiskV1alpha1Client, err error) {

	// getKubeConfig gets config object from config file
	config, err := getKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("Failed to get the kubernetes config: %v", err)
	}

	// generate the clientset extension based off of the config
	azKubeExtensionClientset, err = clientSet.NewForConfig(config)
	if err != nil {
		return azKubeExtensionClientset, fmt.Errorf("Cannot create the clientset: %v", err)
	}

	klog.Info("Successfully constructed kubernetes client and extension clientset")
	return azKubeExtensionClientset, nil
}

// TODO add back with integration tests
// func getKubernetesClientset() (*kubernetes.Clientset, error) {

// 	// getKubeConfig gets config object from config file
// 	config, err := getKubeConfig()
// 	if err != nil {
// 		return nil, fmt.Errorf("Failed to get the kubernetes config: %v", err)
// 	}

// 	// generate the clientset extension based off of the config
// 	kubeClient, err := kubernetes.NewForConfig(config)
// 	if err != nil {
// 		return nil, fmt.Errorf("Cannot create the kubernetes client: %v", err)
// 	}
// 	return kubeClient, nil
// }

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

func getAzDriverNodes(context context.Context, out chan azDriverNodesMeta) {
	// get all nodes that have azDriverNode running
	var activeDriverNodes azDriverNodesMeta
	activeDriverNodes.nodes, activeDriverNodes.err = azDriverNodeExtensionClient.List(context, metav1.ListOptions{})
	out <- activeDriverNodes
}

func getAzVolumeAttachments(context context.Context, out chan azVolumeAttachmentsMeta) {
	// get all azVolumeAttachments running in the cluster
	var activeVolumeAttachments azVolumeAttachmentsMeta
	activeVolumeAttachments.volumes, activeVolumeAttachments.err = azVolumeAttachmentExtensionClient.List(context, metav1.ListOptions{})
	out <- activeVolumeAttachments
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

func setNodeSocresToZero(nodes []v1.Node) (priorityList schedulerapi.HostPriorityList) {
	for _, node := range nodes {
		hostPriority := schedulerapi.HostPriority{Host: node.Name, Score: 0}
		priorityList = append(priorityList, hostPriority)
	}
	return
}
