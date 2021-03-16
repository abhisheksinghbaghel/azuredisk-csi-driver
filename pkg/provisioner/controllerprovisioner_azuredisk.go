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
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2020-12-01/compute"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	azDiskClientSet "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azureutils"
	azure "sigs.k8s.io/cloud-provider-azure/pkg/provider"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

type ControllerProvisioner struct {
	Cloud        *azure.Cloud
	azDiskClient azDiskClientSet.Interface
}

func (c *ControllerProvisioner) ValidateControllerCreateVolumeParameters(params map[string]string) error {
	if azureutils.IsAzureStackCloud(c.Cloud.Config.Cloud, c.Cloud.Config.DisableAzureStackCloud) {
		maxSharesField := "maxSharesField"
		var maxShares int
		var err error
		for k, v := range params {
			if strings.EqualFold(maxSharesField, k) {
				maxShares, err = strconv.Atoi(v)
				if err != nil {
					return status.Error(codes.InvalidArgument, fmt.Sprintf("parse %s failed with error: %v", v, err))
				}
				if maxShares < 1 {
					return status.Error(codes.InvalidArgument, fmt.Sprintf("parse %s returned with invalid value: %d", v, maxShares))
				}

				break
			}
		}
		if maxShares > 1 {
			return status.Error(codes.InvalidArgument, fmt.Sprintf("Invalid maxShares value: %d as Azure Stack does not support shared disk.", maxShares))
		}
	}

	return nil
}

// GetSourceDiskSize recursively searches for the sourceDisk and returns: sourceDisk disk size, error
func (c *ControllerProvisioner) GetSourceDiskSize(ctx context.Context, resourceGroup, diskName string, curDepth, maxDepth int) (*int32, error) {
	if curDepth > maxDepth {
		return nil, status.Error(codes.Internal, fmt.Sprintf("current depth (%d) surpassed the max depth (%d) while searching for the source disk size", curDepth, maxDepth))
	}
	result, rerr := c.Cloud.DisksClient.Get(ctx, resourceGroup, diskName)
	if rerr != nil {
		return nil, rerr.Error()
	}
	if result.DiskProperties == nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("DiskProperty not found for disk (%s) in resource group (%s)", diskName, resourceGroup))
	}

	if result.DiskProperties.CreationData != nil && (*result.DiskProperties.CreationData).CreateOption == "Copy" {
		klog.V(2).Infof("Clone source disk has a parent source")
		sourceResourceID := *result.DiskProperties.CreationData.SourceResourceID
		parentResourceGroup, _ := c.GetResourceGroupFromDiskURI(sourceResourceID)
		parentDiskName := path.Base(sourceResourceID)
		return c.GetSourceDiskSize(ctx, parentResourceGroup, parentDiskName, curDepth+1, maxDepth)
	}

	if (*result.DiskProperties).DiskSizeGB == nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("DiskSizeGB for disk (%s) in resourcegroup (%s) is nil", diskName, resourceGroup))
	}
	return (*result.DiskProperties).DiskSizeGB, nil
}

func (c *ControllerProvisioner) GetValidDiskName(diskName string) string {
	return azureutils.GetValidDiskName(diskName)
}

func (c *ControllerProvisioner) ValidateDiskURI(diskURI string) error {
	return azureutils.IsValidDiskURI(diskURI)
}

func (c *ControllerProvisioner) GetDiskNameFromDiskURI(diskURI string) (string, error) {
	return azureutils.GetDiskNameFromAzureManagedDiskURI(diskURI)
}

func (c *ControllerProvisioner) GetResourceGroupFromDiskURI(diskURI string) (string, error) {
	return azureutils.GetResourceGroupFromAzureManagedDiskURI(diskURI)
}

func (c *ControllerProvisioner) ValidateAndNormalizeStorageAccountType(storageAccountType string) (compute.DiskStorageAccountTypes, error) {
	return azureutils.NormalizeAzureStorageAccountType(storageAccountType, c.Cloud.Config.Cloud, c.Cloud.Config.DisableAzureStackCloud)
}

func (c *ControllerProvisioner) ValidateAndNormalizeCachingMode(cachingMode v1.AzureDataDiskCachingMode) (v1.AzureDataDiskCachingMode, error) {
	return azureutils.NormalizeAzureDataDiskCachingMode(cachingMode)
}

func (c *ControllerProvisioner) CreateDisk(options *CreateDiskOptions) (string, error) {
	diskOptions := &azure.ManagedDiskOptions{
		DiskName:            options.DiskName,
		StorageAccountType:  options.StorageAccountType,
		ResourceGroup:       options.ResourceGroup,
		PVCName:             options.PVCName,
		SizeGB:              options.SizeGB,
		Tags:                options.Tags,
		AvailabilityZone:    options.AvailabilityZone,
		DiskIOPSReadWrite:   options.DiskIOPSReadWrite,
		DiskMBpsReadWrite:   options.DiskMBpsReadWrite,
		SourceResourceID:    options.SourceResourceID,
		SourceType:          options.SourceType,
		DiskEncryptionSetID: options.DiskEncryptionSetID,
		MaxShares:           options.MaxShares,
		LogicalSectorSize:   options.LogicalSectorSize,
	}

	return c.Cloud.CreateManagedDisk(diskOptions)
}

func (c *ControllerProvisioner) DeleteDisk(diskURI string) error {
	return c.Cloud.DeleteManagedDisk(diskURI)
}

func (c *ControllerProvisioner) CheckDiskExists(ctx context.Context, diskURI string) error {
	diskName, err := azureutils.GetDiskNameFromAzureManagedDiskURI(diskURI)
	if err != nil {
		return err
	}

	resourceGroup, err := azureutils.GetResourceGroupFromAzureManagedDiskURI(diskURI)
	if err != nil {
		return err
	}

	if _, rerr := c.Cloud.DisksClient.Get(ctx, resourceGroup, diskName); rerr != nil {
		return rerr.Error()
	}

	return nil
}

func (c *ControllerProvisioner) GetDiskLun(diskName string, diskURI string, nodeName types.NodeName) (int32, error) {
	return c.Cloud.GetDiskLun(diskName, diskURI, nodeName)
}

func (c *ControllerProvisioner) GetCachingMode(attributes map[string]string) (compute.CachingTypes, error) {
	var (
		cachingMode v1.AzureDataDiskCachingMode
		err         error
	)

	cachingModeField := "cachingModeField"

	for k, v := range attributes {
		if strings.EqualFold(k, cachingModeField) {
			cachingMode = v1.AzureDataDiskCachingMode(v)
			break
		}
	}

	cachingMode, err = azureutils.NormalizeAzureDataDiskCachingMode(cachingMode)
	return compute.CachingTypes(cachingMode), err
}

func (c *ControllerProvisioner) AttachDisk(options *AttachDetachDiskOptions) (int32, error) {
	return c.Cloud.AttachDisk(options.IsManagedDisk, options.DiskName, options.DiskURI, options.NodeName, options.CachingMode)
}

func (c *ControllerProvisioner) DetachDisk(options *AttachDetachDiskOptions) error {
	return c.Cloud.DetachDisk(options.DiskName, options.DiskURI, options.NodeName)
}

func (c *ControllerProvisioner) SetIncrementalValueForCreateSnapshot() bool {
	incremental := true
	if azureutils.IsAzureStackCloud(c.Cloud.Config.Cloud, c.Cloud.Config.DisableAzureStackCloud) {
		klog.V(2).Info("Use full snapshot instead as Azure Stack does not incremental snapshot.")
		incremental = false
	}

	return incremental
}

func (c *ControllerProvisioner) CreateOrUpdateSnapshot(options *SnapshotOptions) *retry.Error {
	return c.Cloud.SnapshotsClient.CreateOrUpdate(options.Context, options.ResourceGroup, options.SnapshotName, options.Snapshot)
}

func (c *ControllerProvisioner) DeleteSnapshot(options *SnapshotOptions) *retry.Error {
	return c.Cloud.SnapshotsClient.Delete(options.Context, options.ResourceGroup, options.SnapshotName)
}

func (c *ControllerProvisioner) GetSnapshotInfoByID(snapshotID string) (string, string, error) {
	var (
		snapshotName  string
		resourceGroup string
		err           error
	)

	if azureutils.IsARMResourceID(snapshotID) {
		snapshotName, resourceGroup, err = azureutils.GetSnapshotInfoByID(snapshotID)
	}

	return snapshotName, resourceGroup, err
}

func (c *ControllerProvisioner) GetSnapshotByID(options *SnapshotOptions) (*csi.Snapshot, error) {
	snapshotName, resourceGroupName, err := c.GetSnapshotInfoByID(options.SnapshotName)
	if err != nil {
		return nil, err
	}

	if snapshotName == "" && resourceGroupName == "" {
		snapshotName = options.SnapshotName
		resourceGroupName = options.ResourceGroup
	}

	snapshot, rerr := c.Cloud.SnapshotsClient.Get(options.Context, resourceGroupName, snapshotName)
	if rerr != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("get snapshot %s from rg(%s) error: %v", snapshotName, resourceGroupName, rerr.Error()))
	}

	return azureutils.GenerateCSISnapshot(options.SourceVolumeID, &snapshot)
}

func (c *ControllerProvisioner) ListSnapshotsInCurrentResourceGroup(ctx context.Context) ([]compute.Snapshot, *retry.Error) {
	return c.Cloud.SnapshotsClient.ListByResourceGroup(ctx, c.Cloud.ResourceGroup)
}

func (c *ControllerProvisioner) ListVolumesInCurrentResourceGroup(ctx context.Context, resourceGroup string) ([]compute.Disk, *retry.Error) {
	return c.Cloud.DisksClient.ListByResourceGroup(ctx, resourceGroup)
}

func (c *ControllerProvisioner) GetDiskInfo(ctx context.Context, resourceGroup string, diskName string) (compute.Disk, *retry.Error) {
	return c.Cloud.DisksClient.Get(ctx, resourceGroup, diskName)
}

func (c *ControllerProvisioner) ResizeDisk(diskURI string, oldSize resource.Quantity, requestSize resource.Quantity) (resource.Quantity, error) {
	return c.Cloud.ResizeDisk(diskURI, oldSize, requestSize)
}

func (c *ControllerProvisioner) GetNodeNameByProviderIDInVMSet(providerID string) (types.NodeName, error) {
	return c.Cloud.VMSet.GetNodeNameByProviderID(providerID)
}

func (c *ControllerProvisioner) GetKubeClient() clientset.Interface {
	return c.Cloud.KubeClient
}
