/*
Copyright 2021 The Kubernetes Authors.

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

package provisioner

import (
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azureutils"
)

type ControllerProvisioner struct{}

func NewControllerProvisioner(useAzureStorage bool, kubeConfig *rest.Config, kubeClient clientset.Interface) (*ControllerProvisioner, error) {
	if useAzureStorage {
		klog.V(2).Info("To use the flag when selecting amongst storage provider.")
	}

	diskClient, err := azureutils.GetAzDiskClient(kubeConfig)
	if err != nil {
		return nil, err
	}

	azCloud, err := azureutils.GetAzureCloudProvider(kubeClient)
	if err != nil {
		return nil, err
	}

	return &AzureDiskControllerProvisioner{
		azDiskClient: diskClient,
		Cloud:        azCloud,
	}, nil
}
