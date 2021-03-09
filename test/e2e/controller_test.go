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

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"

	"sigs.k8s.io/azuredisk-csi-driver/pkg/apis/azuredisk/v1alpha1"
	azDiskClientSet "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned"
	"sigs.k8s.io/azuredisk-csi-driver/test/e2e/testsuites"
)

const (
	partitionKey = "azdrivernodes.disk.csi.azure.com/partition"
	provisioner  = "disk.csi.azure.com"
	namespace    = "azure-disk-csi"
)

var _ = ginkgo.Describe("Controller", func() {
	f := framework.NewDefaultFramework("azuredisk")

	var (
		cs           clientset.Interface
		azDiskClient *azDiskClientSet.Clientset
		err          error
	)

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
		azDiskClient, err = azDiskClientSet.NewForConfig(f.ClientConfig())
		if err != nil {
			ginkgo.Fail(fmt.Sprintf("Failed to create disk client. Error: %v", err))
		}
	})

	ginkgo.Context("AzDriverNode", func() {
		ginkgo.It("Should create AzDriverNode resource and report heartbeat.", func() {
			skipIfUsingInTreeVolumePlugin()
			skipIfNotUsingCSIDriverV2()

			pods, err := cs.CoreV1().Pods("kube-system").List(context.TODO(), metav1.ListOptions{})
			framework.ExpectNoError(err)

			for _, pod := range pods.Items {
				if strings.Contains(pod.Spec.NodeName, "csi-azuredisk-node") {
					azN := azDiskClient.DiskV1alpha1().AzDriverNodes("azure-disk-csi")
					dNode, err := azN.Get(context.Background(), pod.Spec.NodeName, metav1.GetOptions{})
					framework.ExpectNoError(err)
					ginkgo.By("Checking AzDriverNode/Staus")
					if dNode.Status == nil {
						ginkgo.Fail("Driver status is not updated")
					}
					ginkgo.By("Checking to see if node is ReadyForVolumeAllocation")
					if dNode.Status.ReadyForVolumeAllocation == nil || *dNode.Status.ReadyForVolumeAllocation != true {
						ginkgo.Fail("Driver found not ready for allocation")
					}
					ginkgo.By("Checking to see if node reported heartbeat")
					if dNode.Status.LastHeartbeatTime == nil || *dNode.Status.LastHeartbeatTime <= 0 {
						ginkgo.Fail("Driver heartbeat not reported")
					}

					ginkgo.By("Checking to see if node has partition key label.")
					partition, ok := dNode.Labels[partitionKey]
					if ok == false || partition == "" {
						ginkgo.Fail("Driver node parition label was not applied correctly.")
					}
					break
				}
			}
		})
	})

	/*
		TODO: Refactor testing codes
	*/
	ginkgo.Context("AzVolumeAttachment", func() {
		ginkgo.It("Should update AzVolumeAttachment object's status from AttachRequired to Attached", func() {
			skipIfUsingInTreeVolumePlugin()
			skipIfNotUsingCSIDriverV2()

			// create test storage class
			// if test storage class already exists delete
			scName := "test-storageclass"
			if _, err := cs.StorageV1().StorageClasses().Get(context.Background(), scName, metav1.GetOptions{}); err == nil {
				err := cs.StorageV1().StorageClasses().Delete(context.Background(), scName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}
			_, err := cs.StorageV1().StorageClasses().Create(context.Background(), &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: scName,
				},
				Provisioner: provisioner,
				Parameters: map[string]string{
					"maxShares": "1",
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// create test az volume
			azVol := azDiskClient.DiskV1alpha1().AzVolumes(namespace)
			volName := "test-vol"
			// if test volume object already exists delete the object
			if _, err := azVol.Get(context.Background(), volName, metav1.GetOptions{}); err == nil {
				err := azVol.Delete(context.Background(), volName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}
			_, err = azVol.Create(context.Background(), &v1alpha1.AzVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: volName,
				},
				Spec: v1alpha1.AzVolumeSpec{
					UnderlyingVolume: volName,
					StorageClass:     scName,
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// create test az node
			azNode := azDiskClient.DiskV1alpha1().AzDriverNodes(namespace)
			nodeName := "test-node"
			if _, err := azNode.Get(context.Background(), nodeName, metav1.GetOptions{}); err == nil {
				err := azNode.Delete(context.Background(), nodeName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}
			_, err = azNode.Create(context.Background(), &v1alpha1.AzDriverNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Spec: v1alpha1.AzDriverNodeSpec{
					NodeName: nodeName,
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// create test az volume attachment
			azAtt := azDiskClient.DiskV1alpha1().AzVolumeAttachments(namespace)
			attName := fmt.Sprintf("%s-%s-attachment", volName, nodeName)
			// if test attachment object already exists delete the object
			if _, err := azAtt.Get(context.Background(), attName, metav1.GetOptions{}); err == nil {
				err := azAtt.Delete(context.Background(), attName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}

			att, err := azAtt.Create(context.Background(), &v1alpha1.AzVolumeAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name: attName,
				},
				Spec: v1alpha1.AzVolumeAttachmentSpec{
					NodeName:         nodeName,
					UnderlyingVolume: volName,
					AttachmentTier:   v1alpha1.Primary,
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			err = testsuites.WaitForAttach(azAtt, att.Namespace, att.Name, time.Duration(5)*time.Minute)
			framework.ExpectNoError(err)

			err = azAtt.Delete(context.Background(), att.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err)
		})
	})

	ginkgo.Context("AzVolumeAttachment", func() {
		ginkgo.It("Should delete AzVolumeAttachment object if it is DetachRequired", func() {
			skipIfUsingInTreeVolumePlugin()
			skipIfNotUsingCSIDriverV2()

			// create test storage class
			// if test storage class already exists delete
			scName := "test-storageclass"
			if _, err := cs.StorageV1().StorageClasses().Get(context.Background(), scName, metav1.GetOptions{}); err == nil {
				err := cs.StorageV1().StorageClasses().Delete(context.Background(), scName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}
			_, err := cs.StorageV1().StorageClasses().Create(context.Background(), &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: scName,
				},
				Provisioner: provisioner,
				Parameters: map[string]string{
					"maxShares": "1",
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// create test az volume
			azVol := azDiskClient.DiskV1alpha1().AzVolumes(namespace)
			volName := "test-vol"
			// if test volume object already exists delete the object
			if _, err := azVol.Get(context.Background(), volName, metav1.GetOptions{}); err == nil {
				err := azVol.Delete(context.Background(), volName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}
			_, err = azVol.Create(context.Background(), &v1alpha1.AzVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: volName,
				},
				Spec: v1alpha1.AzVolumeSpec{
					UnderlyingVolume: volName,
					StorageClass:     scName,
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// create test az node
			azNode := azDiskClient.DiskV1alpha1().AzDriverNodes(namespace)
			nodeName := "test-node"
			if _, err := azNode.Get(context.Background(), nodeName, metav1.GetOptions{}); err == nil {
				err := azNode.Delete(context.Background(), nodeName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}
			_, err = azNode.Create(context.Background(), &v1alpha1.AzDriverNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Spec: v1alpha1.AzDriverNodeSpec{
					NodeName: nodeName,
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// create test az volume attachment
			azAtt := azDiskClient.DiskV1alpha1().AzVolumeAttachments(namespace)
			attName := fmt.Sprintf("%s-%s-attachment", volName, nodeName)
			// if test attachment object already exists delete the object
			if _, err := azAtt.Get(context.Background(), attName, metav1.GetOptions{}); err == nil {
				err := azAtt.Delete(context.Background(), attName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}

			att, err := azAtt.Create(context.Background(), &v1alpha1.AzVolumeAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name: attName,
				},
				Spec: v1alpha1.AzVolumeAttachmentSpec{
					NodeName:         nodeName,
					UnderlyingVolume: volName,
					AttachmentTier:   v1alpha1.Primary,
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			err = testsuites.WaitForAttach(azAtt, att.Namespace, att.Name, time.Duration(5)*time.Minute)
			framework.ExpectNoError(err)

			att, err = azAtt.Get(context.Background(), attName, metav1.GetOptions{})
			framework.ExpectNoError(err)

			att = att.DeepCopy()
			att.Status = att.Status.DeepCopy()
			att.Status.AttachmentState = v1alpha1.DetachRequired
			_, err = azAtt.UpdateStatus(context.Background(), att, metav1.UpdateOptions{})
			framework.ExpectNoError(err)

			err = testsuites.WaitForDelete(azAtt, att.Namespace, att.Name, time.Duration(5)*time.Minute)
			framework.ExpectNoError(err)
		})
	})

	ginkgo.Context("AzVolumeAttachment", func() {
		ginkgo.It("Should create replica azVolumeAttachment object when maxShares > 1", func() {
			skipIfUsingInTreeVolumePlugin()
			skipIfNotUsingCSIDriverV2()

			// create test storage class
			// if test storage class already exists delete
			scName := "test-storageclass"
			if _, err := cs.StorageV1().StorageClasses().Get(context.Background(), scName, metav1.GetOptions{}); err == nil {
				err := cs.StorageV1().StorageClasses().Delete(context.Background(), scName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}
			_, err := cs.StorageV1().StorageClasses().Create(context.Background(), &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: scName,
				},
				Provisioner: provisioner,
				Parameters: map[string]string{
					"maxShares": "2",
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// create test az volume
			azVol := azDiskClient.DiskV1alpha1().AzVolumes(namespace)
			volName := "test-vol"
			// if test volume object already exists delete the object
			if _, err := azVol.Get(context.Background(), volName, metav1.GetOptions{}); err == nil {
				err := azVol.Delete(context.Background(), volName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}
			_, err = azVol.Create(context.Background(), &v1alpha1.AzVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: volName,
				},
				Spec: v1alpha1.AzVolumeSpec{
					UnderlyingVolume: volName,
					StorageClass:     scName,
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// create 2 test az node
			azNode := azDiskClient.DiskV1alpha1().AzDriverNodes(namespace)
			nodeName1, nodeName2 := "test-node-1", "test-node-2"
			if _, err := azNode.Get(context.Background(), nodeName1, metav1.GetOptions{}); err == nil {
				err := azNode.Delete(context.Background(), nodeName1, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}
			_, err = azNode.Create(context.Background(), &v1alpha1.AzDriverNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName1,
				},
				Spec: v1alpha1.AzDriverNodeSpec{
					NodeName: nodeName1,
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			if _, err := azNode.Get(context.Background(), nodeName2, metav1.GetOptions{}); err == nil {
				err := azNode.Delete(context.Background(), nodeName2, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}
			_, err = azNode.Create(context.Background(), &v1alpha1.AzDriverNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName2,
				},
				Spec: v1alpha1.AzDriverNodeSpec{
					NodeName: nodeName2,
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// create test az volume attachment
			azAtt := azDiskClient.DiskV1alpha1().AzVolumeAttachments(namespace)
			attName := fmt.Sprintf("%s-%s-attachment", volName, nodeName1)
			// if test attachment object already exists delete the object
			if _, err := azAtt.Get(context.Background(), attName, metav1.GetOptions{}); err == nil {
				err := azAtt.Delete(context.Background(), attName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}

			att, err := azAtt.Create(context.Background(), &v1alpha1.AzVolumeAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name: attName,
				},
				Spec: v1alpha1.AzVolumeAttachmentSpec{
					NodeName:         nodeName1,
					UnderlyingVolume: volName,
					AttachmentTier:   v1alpha1.Primary,
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// check if the second attachment object was created and marked attached.
			err = testsuites.WaitForReplicas(azAtt, att.Namespace, volName, 1, time.Duration(5)*time.Minute)
			framework.ExpectNoError(err)

			// clean-up
			attachments, err := azAtt.List(context.Background(), metav1.ListOptions{})
			framework.ExpectNoError(err)

			for _, attachment := range attachments.Items {
				if attachment.Spec.UnderlyingVolume == volName {
					err = azAtt.Delete(context.Background(), attachment.Name, metav1.DeleteOptions{})
					framework.ExpectNoError(err)
				}
			}
		})
	})

})
