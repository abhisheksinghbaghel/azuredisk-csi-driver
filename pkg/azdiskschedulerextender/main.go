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
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
	schedulerapi "k8s.io/kube-scheduler/extender/v1"

	clientSet "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned/typed/azuredisk/v1alpha1"
)

func init() {
	klog.InitFlags(nil)
}

var (
	metricsAddress                    = flag.String("metrics-address", "0.0.0.0:29604", "export the metrics")
	azDiskSchedulerExtenderPort       = flag.String("port", "8080", "port used by az scheduler extender")
	kubeClient                        kubernetes.Interface
	azKubeExtensionClient             clientSet.DiskV1alpha1Interface
	azVolumeAttachmentExtensionClient clientSet.AzVolumeAttachmentInterface
	azDriverNodeExtensionClient       clientSet.AzDriverNodeInterface
)

const (
	metricsRequestStr    = "/metrics"
	apiPrefix            = "/azdiskschedulerextender"
	filterRequestStr     = apiPrefix + "/filter"
	prioritizeRequestStr = apiPrefix + "/prioritize"
	pingRequestStr       = "/ping"
)

func init() {

	var err error
	_, azKubeExtensionClient, azVolumeAttachmentExtensionClient, azDriverNodeExtensionClient, err = getKubernetesClient()

	if err != nil {
		klog.Fatalf("Failed to create kubernetes clinetset %s ...", err)
		os.Exit(1)
	}
}

func main() {
	flag.Parse()
	exportMetrics()

	azDiskSchedulerExtenderEndpoint := fmt.Sprintf("%s%s", ":", *azDiskSchedulerExtenderPort)
	klog.V(2).Infof("Starting azdiskschedulerextender on address %s ...", azDiskSchedulerExtenderEndpoint)

	server := &http.Server{Addr: azDiskSchedulerExtenderEndpoint}
	http.HandleFunc(prioritizeRequestStr, handlePrioritizeRequest)
	http.HandleFunc(filterRequestStr, handleFilterRequest)
	http.HandleFunc(pingRequestStr, handlePingRequest)
	http.HandleFunc("/", handleUnknownRequest)

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		klog.Fatalf("Exiting. Error starting extender server: %v", err)
	}
	klog.V(2).Infof("Exiting azdiskschedulerextender ...")
	os.Exit(0)
}

func handleUnknownRequest(response http.ResponseWriter, request *http.Request) {
	klog.Errorf("handleUnknownRequest: Not implemented request received. Request: %s . Status: %v", request.URL.Path, http.StatusNotImplemented)
	http.Error(response, "Not implemented request received. Request: "+request.URL.Path, http.StatusNotImplemented)
}

func handlePingRequest(response http.ResponseWriter, request *http.Request) {
	// TODO: Return an error code based on resource metrics
	// ex. CPU\Memory usage
	started := time.Now()
	duration := time.Since(started)
	if duration.Seconds() > 10 {
		response.WriteHeader(500)
		_, _ = response.Write([]byte(fmt.Sprintf("error: %v", duration.Seconds())))
		klog.V(2).Infof("handlePingRequest: Received ping request. Responding with 500.")
	} else {
		response.WriteHeader(200)
		_, _ = response.Write([]byte("ok"))
		klog.V(2).Infof("handlePingRequest: Received ping request. Responding with 200.")
	}
}

func handleFilterRequest(response http.ResponseWriter, request *http.Request) {
	var (
		args              schedulerapi.ExtenderArgs
		filteredNodes     []v1.Node
		filteredNodeNames []string
		failedNodes       map[string]string
	)

	if request.Body == nil {
		klog.Errorf("handleFilterRequest: Error request body is empty.")
		http.Error(response, "Error request body is empty", http.StatusBadRequest)
		return
	}

	decoder := json.NewDecoder(request.Body)
	encoder := json.NewEncoder(response)

	if err := decoder.Decode(&args); err != nil {
		klog.Errorf("handleFilterRequest: Error decoding filter request: %v", err)
		http.Error(response, "Decode error", http.StatusBadRequest)
		return
	}

	allNodes := args.Nodes.Items
	if len(allNodes) == 0 {
		klog.Errorf("handleFilterRequest: No nodes received in filter request")
		http.Error(response, "Bad request", http.StatusBadRequest)
		return
	}
	azDriverNodes, _ := azDriverNodeExtensionClient.List(context.TODO(), metav1.ListOptions{})

	nodeNameToStatusMap := make(map[string]string)
	for _, azDriverNode := range azDriverNodes.Items {
		nodeNameToStatusMap[azDriverNode.Spec.NodeName] = azDriverNode.Status.State
	}

	// Filter the nodes based on AzDiverNode CRI status
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

	responseBody := getFilterResponseBody(filteredNodes, filteredNodeNames, failedNodes, "")
	if err := encoder.Encode(responseBody); err != nil {
		klog.Errorf("handleFilterRequest: Error encoding filter response: %+v : %v", responseBody, err)
	}
}

func handlePrioritizeRequest(response http.ResponseWriter, request *http.Request) {
	var (
		args     schedulerapi.ExtenderArgs
		respList schedulerapi.HostPriorityList
	)
	if request.Body == nil {
		klog.Errorf("handlePrioritizeRequest: Error request body is empty.")
		http.Error(response, "Error request body is empty", http.StatusBadRequest)
		return
	}

	decoder := json.NewDecoder(request.Body)
	encoder := json.NewEncoder(response)
	if err := decoder.Decode(&args); err != nil {
		klog.Errorf("handlePrioritizeRequest: Error decoding prioritize request: %v", err)
		http.Error(response, "Decode error", http.StatusBadRequest)
		return
	}

	//TODO add RWM volume case here
	if args.Pod.Spec.Volumes == nil {
		for _, node := range args.Nodes.Items {
			hostPriority := schedulerapi.HostPriority{Host: node.Name, Score: 0}
			respList = append(respList, hostPriority)
		}
	} else {
		volumesPodNeeds := make(map[string]bool)
		nodeNameToVolumeMap := make(map[string][]string)
		nodeNameToHeartbeatMap := make(map[string]string)
		for _, volume := range args.Pod.Spec.Volumes {
			volumesPodNeeds[volume.Name] = true
		}

		azDriverNodes, _ := azDriverNodeExtensionClient.List(context.TODO(), metav1.ListOptions{})
		for _, azDriverNode := range azDriverNodes.Items {
			nodeNameToHeartbeatMap[azDriverNode.Spec.NodeName] = azDriverNode.Spec.Heartbeat
		}

		azVolumeAttachment, _ := azVolumeAttachmentExtensionClient.List(context.TODO(), metav1.ListOptions{})
		for _, attachedVolume := range azVolumeAttachment.Items {
			_, needs := volumesPodNeeds[attachedVolume.Spec.AzVolumeName]
			if needs {
				nodeNameToVolumeMap[attachedVolume.Spec.AzDriverNodeName] = append(nodeNameToVolumeMap[attachedVolume.Spec.AzDriverNodeName], attachedVolume.Name)
			}
		}

		klog.V(2).Infof("handlePrioritizeRequest: Nodes for pod %+v in response:", args.Pod)
		for _, node := range args.Nodes.Items {
			score := getNodeScore(len(nodeNameToVolumeMap[node.Name]), nodeNameToHeartbeatMap[node.Name])
			hostPriority := schedulerapi.HostPriority{Host: node.Name, Score: score}
			respList = append(respList, hostPriority)
			klog.V(2).Infof("handlePrioritizeRequest: %+v", node)
		}
	}

	if err := encoder.Encode(respList); err != nil {
		klog.Errorf("handlePrioritizeRequest: Failed to encode response: %v", err)
	}
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

func exportMetrics() {
	l, err := net.Listen("tcp", *metricsAddress)
	if err != nil {
		klog.Warningf("failed to get listener for metrics endpoint: %v", err)
		return
	}
	serve(context.Background(), l, serveMetrics)
}

func serve(ctx context.Context, l net.Listener, serveFunc func(net.Listener) error) {
	path := l.Addr().String()
	klog.V(2).Infof("set up prometheus server on %v", path)
	go func() {
		defer l.Close()
		if err := serveFunc(l); err != nil {
			klog.Fatalf("serve failure(%v), address(%v)", err, path)
		}
	}()
}

func serveMetrics(l net.Listener) error {
	m := http.NewServeMux()
	m.Handle(metricsRequestStr, legacyregistry.Handler()) //nolint, because azure cloud provider uses legacyregistry currently
	return trapClosedConnErr(http.Serve(l, m))
}

func trapClosedConnErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "use of closed network connection") {
		return nil
	}
	return err
}

func getKubernetesClient() (kubernetes.Interface, clientSet.DiskV1alpha1Interface, clientSet.AzVolumeAttachmentInterface, clientSet.AzDriverNodeInterface, error) {

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
			return nil, nil, nil, nil, fmt.Errorf("Cannot load kubeconfig: %v", err)
		}
	}

	// generate the client based off of the config
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("Cannot create kubernetes client: %v", err)
	}

	// generate the clientset extension based off of the config
	azKubeExtensionClient, err := clientSet.NewForConfig(config)
	if err != nil {
		return kubeClient, nil, nil, nil, fmt.Errorf("Cannot create clientset: %v", err)
	}
	azVolumeAttachmentExtensionClient := azKubeExtensionClient.AzVolumeAttachments("")
	azDriverNodeExtensionClient := azKubeExtensionClient.AzDriverNodes("")

	klog.Info("Successfully constructed kubernetes client and extension clientset")
	return kubeClient, azKubeExtensionClient, azVolumeAttachmentExtensionClient, azDriverNodeExtensionClient, nil
}

func getFilterResponseBody(filteredNodes []v1.Node, nodeNames []string, failedNodes map[string]string, errorMessage string) *schedulerapi.ExtenderFilterResult {
	return &schedulerapi.ExtenderFilterResult{
		Nodes: &v1.NodeList{
			Items: filteredNodes,
		},
		NodeNames:   &nodeNames,
		FailedNodes: failedNodes,
		Error:       errorMessage,
	}
}
