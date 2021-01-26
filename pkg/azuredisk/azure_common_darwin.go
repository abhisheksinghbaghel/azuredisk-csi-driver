// +build darwin

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

package azuredisk

import (
	"fmt"

	"k8s.io/mount-utils"
)

// Note: This file is added only to ensure that the UTs can be run from MacOS.
func scsiHostRescan(io ioHandler, m *mount.SafeFormatAndMount) {
}

func formatAndMount(source, target, fstype string, options []string, m *mount.SafeFormatAndMount) error {
	return nil
}

func findDiskByLun(lun int, io ioHandler, m *mount.SafeFormatAndMount) (string, error) {
	return "", fmt.Errorf("findDiskByLun not implemented")
}

func preparePublishPath(path string, m *mount.SafeFormatAndMount) error {
	return nil
}

func CleanupMountPoint(path string, m *mount.SafeFormatAndMount, extensiveCheck bool) error {
	return nil
}
