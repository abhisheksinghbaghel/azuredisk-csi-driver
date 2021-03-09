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

package controller

import (
	"context"
	"fmt"

	"sort"
	"strconv"

	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/apis/azuredisk/v1alpha1"
	azVolumeClientSet "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

type reconcileAzVolumeAttachment struct {
	client         client.Client
	azVolumeClient azVolumeClientSet.Interface
	namespace      string
	// TODO look for ways to better utilize memory
	replicaMap map[string]map[string]v1alpha1.AzVolumeAttachment
	primaryMap map[string]v1alpha1.AzVolumeAttachment
	nodeMap    map[string]map[string]v1alpha1.AzVolumeAttachment
	scMap      map[string]map[string]bool
	volMap     map[string]string
}

type filteredNode struct {
	azDriverNode v1alpha1.AzDriverNode
	numAttached  int
}

var _ reconcile.Reconciler = &reconcileAzVolumeAttachment{}

func (r *reconcileAzVolumeAttachment) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if !r.IsInitialized() {
		if err := r.SyncStorageClass(ctx); err != nil {
			return reconcile.Result{Requeue: true}, err
		}
		if err := r.SyncVolumeAttachments(ctx); err != nil {
			return reconcile.Result{Requeue: true}, err
		}
		klog.Info("Successfully initialized maps for azVolumeAttachment Controller")
	}
	// check if request name involves change in storage class
	// TODO: need to know what kind of error would be returned if r.client.get is called on a wrong resource type
	klog.Infof("name: %s, namespace:%s", request.Name, request.Namespace)
	result, err := r.HandleReplicaUpdate(ctx, request)
	if err != nil && errors.IsNotFound(err) {
		// if not assume that an event occurred in azVolumeAttachment object
		return r.HandleAzVolumeAttachmentEvent(ctx, request)
	}
	return result, err
}

func (r *reconcileAzVolumeAttachment) HandleAzVolumeAttachmentEvent(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	var azVolumeAttachment v1alpha1.AzVolumeAttachment
	if err := r.client.Get(ctx, request.NamespacedName, &azVolumeAttachment); err != nil {
		// if the azVolumeAttachment object can no longer be found, it's been deleted so do not requeue
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		// if the GET failure is triggered by other errors, log it and requeue the request
		klog.Errorf("failed to fetch azvolumeattachment object with namespaced name %s: (%v)", request.NamespacedName, err)
		return reconcile.Result{Requeue: true}, err
	}

	// this is a creation event
	if azVolumeAttachment.Status == nil {
		// First, attach the volume to the specified node
		if err := r.AttachVolume(ctx, azVolumeAttachment.Spec.UnderlyingVolume, azVolumeAttachment.Spec.NodeName); err != nil {
			klog.Errorf("failed to attach volume %s to node %s: (%v)", azVolumeAttachment.Spec.UnderlyingVolume, azVolumeAttachment.Spec.NodeName, err)
			return reconcile.Result{Requeue: true}, err
		}

		// If it is the first time seeing this underlying volume, sync scMap and volMap
		if _, ok := r.volMap[azVolumeAttachment.Spec.UnderlyingVolume]; !ok {
			if err := r.SyncStorageClass(ctx); err != nil {
				return reconcile.Result{Requeue: true}, err
			}
		}

		// update the status
		updated := azVolumeAttachment.DeepCopy()
		updated.Status = &v1alpha1.AzVolumeAttachmentStatus{}
		updated.Status.AttachmentTier = updated.Spec.AttachmentTier
		updated.Status.AttachmentState = v1alpha1.Attached

		if azVolumeAttachment.Spec.AttachmentTier == v1alpha1.Primary {
			primary, primaryOk := r.primaryMap[azVolumeAttachment.Spec.UnderlyingVolume]
			// this is a case when previous primary attachment object had not been deleted for some reason,
			// in this case, mark the old primary detach required, and update the primary map
			if primaryOk && primary.Name != azVolumeAttachment.Name {
				if err := r.TriggerDetach(ctx, primary); err != nil {
					// if detach failed, requeue
					return reconcile.Result{Requeue: true}, err
				}
				// this is a case when the attachment object is created for the first time for the specified underlying volume
				// in this case, create n replicas
			} else {
				if err := r.ManageReplicaForVolume(ctx, azVolumeAttachment.Spec.UnderlyingVolume); err != nil {
					return reconcile.Result{Requeue: true}, err
				}
			}
		}

		if _, err := r.azVolumeClient.DiskV1alpha1().AzVolumeAttachments(r.namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
			klog.Errorf("failed to update status for azvolumeattachment object %s: (%v)", azVolumeAttachment.Name, err)
			return reconcile.Result{Requeue: true}, err
		}

		// if status update was successful add the object to either the replica or primary map
		if azVolumeAttachment.Spec.AttachmentTier == v1alpha1.Primary {
			r.primaryMap[azVolumeAttachment.Spec.UnderlyingVolume] = azVolumeAttachment
		} else {
			r.replicaMap[azVolumeAttachment.Spec.UnderlyingVolume][azVolumeAttachment.Spec.NodeName] = azVolumeAttachment
		}

		// If it is the first time seeing this node, sync node map
		if _, ok := r.nodeMap[azVolumeAttachment.Spec.NodeName]; !ok {
			r.nodeMap[azVolumeAttachment.Spec.NodeName] = make(map[string]v1alpha1.AzVolumeAttachment)
		}
		r.nodeMap[azVolumeAttachment.Spec.NodeName][azVolumeAttachment.Spec.UnderlyingVolume] = azVolumeAttachment
		klog.Infof("successfully attached volume %s to node %s and update %s's status", azVolumeAttachment.Spec.UnderlyingVolume, azVolumeAttachment.Spec.NodeName, azVolumeAttachment.Name)
		// if the azVolumeAttachment's attachment state is detach required, detach the disk from the node and delete the object
	} else if azVolumeAttachment.Status.AttachmentState == v1alpha1.DetachRequired {
		if err := r.TriggerDetach(ctx, azVolumeAttachment); err != nil {
			// if detach failed, requeue the request
			return reconcile.Result{Requeue: true}, err
		}
		// And create replica if necessary
		if err := r.ManageReplicaForVolume(ctx, azVolumeAttachment.Spec.UnderlyingVolume); err != nil {
			return reconcile.Result{Requeue: true}, err
		}
		// if the attachmentTier in status and spec are different, it is an update event where replica should be turned into a primary
	} else if azVolumeAttachment.Spec.AttachmentTier != azVolumeAttachment.Status.AttachmentTier {
		if azVolumeAttachment.Spec.AttachmentTier == v1alpha1.Primary {
			// if primary exists for the specified volume, trigger detach for that primary
			if primary, ok := r.primaryMap[azVolumeAttachment.Spec.UnderlyingVolume]; ok && primary.Name != azVolumeAttachment.Name {
				if err := r.TriggerDetach(ctx, primary); err != nil {
					return reconcile.Result{Requeue: true}, err
				}
			}
			// update the replicaMap to avoid potential sync issue in `ManageReplicaForVolume` call
			delete(r.replicaMap[azVolumeAttachment.Spec.UnderlyingVolume], azVolumeAttachment.Spec.NodeName)

			if err := r.ManageReplicaForVolume(ctx, azVolumeAttachment.Spec.UnderlyingVolume); err != nil {
				return reconcile.Result{Requeue: true}, err
			}

			// update the status's attachment tier to primary
			updated := azVolumeAttachment.DeepCopy()
			updated.Status = azVolumeAttachment.Status.DeepCopy()
			updated.Status.AttachmentTier = v1alpha1.Primary
			if _, err := r.azVolumeClient.DiskV1alpha1().AzVolumeAttachments(r.namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
				klog.Errorf("failed to update status of azVolumeAttachment object: (%v)", err)
				return reconcile.Result{Requeue: true}, err
			}
			r.primaryMap[azVolumeAttachment.Spec.UnderlyingVolume] = azVolumeAttachment
		}
	}
	return reconcile.Result{Requeue: true}, nil
}

func (r *reconcileAzVolumeAttachment) HandleReplicaUpdate(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	storageClassName := request.Name
	replicaCount, err := r.GetReplicaCount(ctx, storageClassName)
	if err != nil {
		return reconcile.Result{}, err
	}

	for volume := range r.scMap[storageClassName] {
		if int(replicaCount) != len(r.scMap[storageClassName]) {
			err := r.ManageReplicaForVolume(ctx, volume)
			if err != nil {
				return reconcile.Result{Requeue: true}, err
			}
		}
	}
	return reconcile.Result{}, nil
}

func (r *reconcileAzVolumeAttachment) GetReplicaCount(ctx context.Context, name string) (uint64, error) {
	var storageClass storagev1.StorageClass
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, &storageClass); err != nil {
		// suppress error message
		return 0, err
	}
	var replicaCount uint64
	var err error
	if storageClass.Provisioner != DriverName {
		return 0, errors.NewBadRequest(fmt.Sprintf("storageClass provisioner is required to be %s", DriverName))
	}
	// maxMountReplicaCount can either be explicitly defined or implicitly deduced from maxShares
	if maxMountReplicaCount, ok := storageClass.Parameters["maxMountReplicaCount"]; ok {
		if replicaCount, err = strconv.ParseUint(maxMountReplicaCount, 10, 64); err != nil {
			klog.Errorf("failed to parse maxMountReplicaCount: (%v)", err)
			// if the parameter for maxMountReplicaCount is not formatted properly, there is no point in requeueing the request
			return 0, err
		}
	} else if maxShares, ok := storageClass.Parameters["maxShares"]; ok {
		if replicaCount, err = strconv.ParseUint(maxShares, 10, 64); err != nil {
			klog.Errorf("failed to parse maxMountReplicaCount: (%v)", err)
			// if the parameter for maxMountReplicaCount is not formatted properly, there is no point in requeueing the request
			return 0, err
		}
		replicaCount--
		// if maxShares field does not exist, the storage class is not supported by azVolumeAttachment Controller
	} else {
		klog.Errorf("unable to find maxShares in storage class %s's parameters", name)
		return 0, errors.NewBadRequest("maxShares field is required")
	}
	return replicaCount, nil
}

func (r *reconcileAzVolumeAttachment) ManageReplicaForVolume(ctx context.Context, volume string) error {
	// fetch the replica azVolumeAttachments for the specified underlying volume
	replicas, ok := r.replicaMap[volume]
	if !ok {
		r.replicaMap[volume] = make(map[string]v1alpha1.AzVolumeAttachment)
		replicas = r.replicaMap[volume]
	}
	storageClassName, ok := r.volMap[volume]
	if !ok {
		// this warning message should never be reached, otherwise, there is a sync issue with the maps used for optimization
		klog.Warning(fmt.Sprintf("storage class not registerd for volume %s.", volume))
		return nil
	}
	if replicaCount, err := r.GetReplicaCount(ctx, storageClassName); err != nil {
		return err
		// if the number of existing replicas is smaller than the specified number of maxReplicas, create more azVolumetAttachment object
	} else if int(replicaCount) > len(replicas) {
		nodes, err := r.GetNodesForReplica(ctx, int(replicaCount)-len(replicas), false)
		if err != nil {
			klog.Errorf("failed to get a list of nodes for replica attachment: (%v)", err)
			return err
		}
		for _, node := range nodes {
			_, err := r.azVolumeClient.DiskV1alpha1().AzVolumeAttachments(r.namespace).Create(ctx, &v1alpha1.AzVolumeAttachment{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("%s-%s-attachment", volume, node.azDriverNode.Spec.NodeName),
				},
				Spec: v1alpha1.AzVolumeAttachmentSpec{
					NodeName:         node.azDriverNode.Spec.NodeName,
					UnderlyingVolume: volume,
					AttachmentTier:   v1alpha1.Replica,
				},
			}, metav1.CreateOptions{})
			if err != nil {
				klog.Errorf("failed to create replica azVolumeAttachment for volume %s: (%v)", volume, err)
				return err
			}
			// replica map will be updated in the next round of reconciliation when the volume successfully attaches to the node
		}
		// if the number of existing replicas is larger than the specified number of maxReplicas, trigger a deletion of azVolumeAttachment object with Replica Status
	} else if len(replicas) > int(replicaCount) {
		// TODO: add heuristic to determine node detachment priority
		i := 0
		for _, attachment := range replicas {
			if i >= len(replicas)-int(replicaCount) {
				break
			}
			// if the volume has not yet been attached to any node, it cannot be detached so skip
			if attachment.Status == nil {
				continue
			}
			// otherwise mark the attachment DetachRequired and increment the counter
			updated := attachment.DeepCopy()
			updated.Status.AttachmentState = v1alpha1.DetachRequired
			_, err := r.azVolumeClient.DiskV1alpha1().AzVolumeAttachments(r.namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
			if err != nil {
				klog.Errorf("failed to update volume attachment on node %s with underlying volume %s: (%v)", updated.Spec.NodeName, updated.Spec.UnderlyingVolume, err)
				return err
			}
			i++
		}
	}
	return nil
}

func (r *reconcileAzVolumeAttachment) GetNodesForReplica(ctx context.Context, numReplica int, reverse bool) ([]filteredNode, error) {
	filteredNodes := []filteredNode{}
	nodes, err := r.azVolumeClient.DiskV1alpha1().AzDriverNodes(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("failed to retrieve azDriverNode List for namespace %s: (%v)", r.namespace, err)
		return filteredNodes, err
	}
	if nodes != nil {
		for _, node := range nodes.Items {
			// if attachments cannot be found for a specified node, it is a new AzDriverNode
			if attachments, ok := r.nodeMap[node.Spec.NodeName]; !ok {
				filteredNodes = append(filteredNodes, filteredNode{azDriverNode: node, numAttached: 0})
			} else {
				filteredNodes = append(filteredNodes, filteredNode{azDriverNode: node, numAttached: len(attachments)})
			}
		}
	}

	// sort the filteredNodes by their number of attachments (low to high) and return a slice
	sort.Slice(filteredNodes[:], func(i, j int) bool {
		if reverse {
			return filteredNodes[i].numAttached > filteredNodes[j].numAttached

		}
		return filteredNodes[i].numAttached < filteredNodes[j].numAttached
	})

	if len(filteredNodes) > numReplica {
		return filteredNodes[:numReplica], nil
	}
	return filteredNodes, nil
}

// Deprecated
func (r *reconcileAzVolumeAttachment) ListReplicasByVolume(ctx context.Context, volume string) ([]v1alpha1.AzVolumeAttachment, error) {
	replicas := []v1alpha1.AzVolumeAttachment{}
	attachments, err := r.azVolumeClient.DiskV1alpha1().AzVolumeAttachments(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("failed to get attachment list for namespace %s: %v", r.namespace, err)
		return replicas, err
	}
	if attachments != nil {
		for _, attachment := range attachments.Items {
			if attachment.Spec.UnderlyingVolume == volume && attachment.Spec.AttachmentTier == v1alpha1.Replica {
				replicas = append(replicas, attachment)
			}
		}
	}
	return replicas, nil
}

func (r *reconcileAzVolumeAttachment) ListAzVolumeAttachmentsByNodeName(ctx context.Context, nodeName string) ([]v1alpha1.AzVolumeAttachment, error) {
	filteredAttachments := []v1alpha1.AzVolumeAttachment{}
	attachments, err := r.azVolumeClient.DiskV1alpha1().AzVolumeAttachments(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("failed to get attachment list for namespace %s: %v", r.namespace, err)
		return filteredAttachments, err
	}
	if attachments != nil {
		for _, attachment := range attachments.Items {
			if attachment.Spec.NodeName == nodeName {
				filteredAttachments = append(filteredAttachments, attachment)
			}
		}
	}
	return filteredAttachments, nil
}

func (r *reconcileAzVolumeAttachment) SyncStorageClass(ctx context.Context) error {
	if r.scMap == nil {
		r.scMap = make(map[string]map[string]bool)
	}
	if r.volMap == nil {
		r.volMap = make(map[string]string)
	}

	volumes, err := r.azVolumeClient.DiskV1alpha1().AzVolumes(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("failed to retrieve list of azVolume objects in namespace %s: (%v)", r.namespace, err)
		return err
	}

	for _, volume := range volumes.Items {
		if _, ok := r.scMap[volume.Spec.StorageClass]; !ok {
			r.scMap[volume.Spec.StorageClass] = make(map[string]bool)
		}
		if ok := r.scMap[volume.Spec.StorageClass][volume.Spec.UnderlyingVolume]; !ok {
			r.scMap[volume.Spec.StorageClass][volume.Spec.UnderlyingVolume] = true
		}
		if _, ok := r.volMap[volume.Spec.UnderlyingVolume]; !ok {
			r.volMap[volume.Spec.UnderlyingVolume] = volume.Spec.StorageClass
		}
	}
	return nil
}

func (r *reconcileAzVolumeAttachment) SyncVolumeAttachments(ctx context.Context) error {
	// initialize replica map, primary map, and node map
	r.replicaMap = make(map[string]map[string]v1alpha1.AzVolumeAttachment)
	r.primaryMap = make(map[string]v1alpha1.AzVolumeAttachment)
	r.nodeMap = make(map[string]map[string]v1alpha1.AzVolumeAttachment)

	attachments, err := r.azVolumeClient.DiskV1alpha1().AzVolumeAttachments(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil && errors.IsNotFound(err) {
		klog.Errorf("failed to retrieve azVolumeAttachment list in namespace %s: (%v)", r.namespace, err)
		return errors.NewInternalError(err)
	}
	if attachments != nil {
		for _, attachment := range attachments.Items {
			if _, exists := r.nodeMap[attachment.Spec.NodeName]; !exists {
				r.nodeMap[attachment.Spec.NodeName] = make(map[string]v1alpha1.AzVolumeAttachment)
			}
			r.nodeMap[attachment.Spec.NodeName][attachment.Spec.UnderlyingVolume] = attachment
			switch tier := attachment.Spec.AttachmentTier; tier {
			case v1alpha1.Primary:
				r.primaryMap[attachment.Spec.UnderlyingVolume] = attachment
			case v1alpha1.Replica:
				if _, exists := r.replicaMap[attachment.Spec.UnderlyingVolume]; !exists {
					r.replicaMap[attachment.Spec.UnderlyingVolume] = make(map[string]v1alpha1.AzVolumeAttachment)
				}
				r.replicaMap[attachment.Spec.UnderlyingVolume][attachment.Spec.NodeName] = attachment
			default:
				// should never be reached
			}
		}
	}
	return nil
}

func (r *reconcileAzVolumeAttachment) IsInitialized() bool {
	return r.replicaMap != nil && r.primaryMap != nil && r.scMap != nil && r.volMap != nil && r.nodeMap != nil
}

func (r *reconcileAzVolumeAttachment) TriggerDetach(ctx context.Context, azVolumeAttachment v1alpha1.AzVolumeAttachment) error {
	if err := r.DetachVolume(ctx, azVolumeAttachment.Spec.UnderlyingVolume, azVolumeAttachment.Spec.NodeName); err != nil {
		klog.Errorf("failed to detach volume %s from node %s: (%v)", azVolumeAttachment.Spec.UnderlyingVolume, azVolumeAttachment.Spec.NodeName, err)
		return err
	}
	// If detach was successful, delete azVolumeAttachment object and update the map
	err := r.azVolumeClient.DiskV1alpha1().AzVolumeAttachments(r.namespace).Delete(ctx, azVolumeAttachment.Name, metav1.DeleteOptions{})
	if err != nil {
		klog.Errorf("failed to delete azvolumeattachment %s: (%v)", azVolumeAttachment.Name, err)
		return err
	}
	if azVolumeAttachment.Status.AttachmentTier == v1alpha1.Primary {
		delete(r.primaryMap, azVolumeAttachment.Spec.UnderlyingVolume)
	} else {
		delete(r.replicaMap[azVolumeAttachment.Spec.UnderlyingVolume], azVolumeAttachment.Spec.NodeName)
	}
	delete(r.nodeMap[azVolumeAttachment.Spec.NodeName], azVolumeAttachment.Spec.UnderlyingVolume)
	klog.Infof("successfully detached volume %s from node %s and deleted %s", azVolumeAttachment.Spec.UnderlyingVolume, azVolumeAttachment.Spec.NodeName, azVolumeAttachment.Name)
	return nil
}

func (r *reconcileAzVolumeAttachment) AttachVolume(ctx context.Context, volume, node string) error {
	// TO DO: add attach volume function from provisioner library
	return nil
}

func (r *reconcileAzVolumeAttachment) DetachVolume(ctx context.Context, volume, node string) error {
	// TODO: add detach volume function from provisioner library
	return nil
}

func NewAzVolumeAttachmentController(ctx context.Context, mgr manager.Manager, azVolumeClient *azVolumeClientSet.Interface, namespace string) error {
	c, err := controller.New("azvolumeattachment-controller", mgr, controller.Options{
		MaxConcurrentReconciles: 10,
		Reconciler:              &reconcileAzVolumeAttachment{client: mgr.GetClient(), azVolumeClient: *azVolumeClient, namespace: namespace},
		Log:                     mgr.GetLogger().WithValues("controller", "azdrivernode"),
	})

	if err != nil {
		klog.Errorf("failed to create a new azvolumeattachment controller: (%v)", err)
		return err
	}

	// Watch for CRUD events on azVolumeAttachment objects
	err = c.Watch(&source.Kind{Type: &v1alpha1.AzVolumeAttachment{}}, &handler.Funcs{
		CreateFunc: func(e event.CreateEvent, q workqueue.RateLimitingInterface) {
			if e.Object == nil {
				klog.Warning("received empty object")
				return
			}
			q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
				Name:      e.Object.GetName(),
				Namespace: e.Object.GetNamespace(),
			}})
		},
		UpdateFunc: func(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
			if e.ObjectNew == nil || e.ObjectOld == nil {
				return
			}
			// if it is an update in state from AttachRequired to Attached, ignore
			go func() {
				var oldAttachment, newAttachment v1alpha1.AzVolumeAttachment
				err1 := mgr.GetClient().Get(ctx, types.NamespacedName{Name: e.ObjectOld.GetName(), Namespace: e.ObjectOld.GetNamespace()}, &oldAttachment)
				err2 := mgr.GetClient().Get(ctx, types.NamespacedName{Name: e.ObjectNew.GetName(), Namespace: e.ObjectNew.GetNamespace()}, &newAttachment)
				if err1 != nil || err2 != nil {
					return
				}
				if oldAttachment.Status.AttachmentState == v1alpha1.AttachRequired && newAttachment.Status.AttachmentState == v1alpha1.Attached {
					return
				}
				q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
					Name:      e.ObjectNew.GetName(),
					Namespace: e.ObjectNew.GetNamespace(),
				}})
			}()
		}})

	if err != nil {
		klog.Errorf("failed to initialize watch for azvolumeattachment object: (%v)", err)
		return err
	}

	// Watch for Update events on storage class objects (managed-csi-shared-with-replica explicitly)
	err = c.Watch(&source.Kind{Type: &storagev1.StorageClass{}}, &handler.Funcs{
		UpdateFunc: func(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
			// only add to the queue if the object's provisioner is disk.csi.azure.com and has MaxShares parameter
			go func() {
				var storageClass storagev1.StorageClass
				if err := mgr.GetClient().Get(ctx, types.NamespacedName{Name: e.ObjectNew.GetName(), Namespace: e.ObjectNew.GetNamespace()}, &storageClass); err != nil {
					return
				} else if _, ok := storageClass.Parameters["maxShares"]; storageClass.Provisioner == DriverName && ok {
					q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
						Name:      e.ObjectNew.GetName(),
						Namespace: e.ObjectNew.GetNamespace(),
					}})
				}
			}()
		},
	})

	if err != nil {
		klog.Errorf("failed to initialize watch for storage class: (%v)", err)
		return err
	}

	klog.V(2).Info("AzVolumeAttachment Controller successfully initialized.")
	return nil
}
