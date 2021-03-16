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

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2020-12-01/compute"
	"k8s.io/apimachinery/pkg/types"
)

type CreateDiskOptions struct {
	DiskName            string
	StorageAccountType  compute.DiskStorageAccountTypes
	PVCName             string
	ResourceGroup       string
	SizeGB              int
	Tags                map[string]string
	AvailabilityZone    string
	DiskIOPSReadWrite   string
	DiskMBpsReadWrite   string
	SourceResourceID    string
	SourceType          string
	DiskEncryptionSetID string
	MaxShares           int32
	LogicalSectorSize   int32
}

type AttachDetachDiskOptions struct {
	IsManagedDisk bool
	DiskName      string
	DiskURI       string
	NodeName      types.NodeName
	CachingMode   compute.CachingTypes
}

type SnapshotOptions struct {
	Context        context.Context
	ResourceGroup  string
	SnapshotName   string
	SourceVolumeID string
	Snapshot       compute.Snapshot
}
