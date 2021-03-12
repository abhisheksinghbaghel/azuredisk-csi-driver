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
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest`"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azureutils"

	azDiskClientSet "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned"
	azure "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

type ControllerProvisioner struct{}

type AzureControllerProvisioner struct {
	azCloud      *azure.Cloud
	azDiskClient azDiskClientSet.Interface
}

type GenericControllerProvisioner struct{}

func NewControllerProvisioner(useAzureStorage bool, kubeConfig *rest.config, kubeClient clientset.Interface) (*ControllerProvisioner, error) {
	if useAzureStorage {
		return &AzureControllerProvisioner{
			azDiskClient: azureutils.GetAzDiskClient(kubeConfig),
			azCloud:      azureutils.GetCloudProvider(kubeClient),
		}, nil
	} else {
		return &GenericControllerProvisioner{}, nil
	}
}

func (c *AzureControllerProvisioner) ValidateCreateVolumeRequestForStorageProvider(req *csi.CreateVolumeRequest) (bool, error) {
	if azureutils.IsAzureStackCloud(d.cloud.Config.Cloud, d.cloud.Config.DisableAzureStackCloud) {
		if maxShares > 1 {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Invalid maxShares value: %d as Azure Stack does not support shared disk.", maxShares))
		}
	}

	if _, err = normalizeCachingMode(cachingMode); err != nil {
		return nil, err
	}

	if ok, err := d.checkDiskCapacity(ctx, resourceGroup, diskName, requestGiB); !ok {
		return nil, err
	}
}
