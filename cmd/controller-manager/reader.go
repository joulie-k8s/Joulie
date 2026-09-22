package main

import (
	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// podNodeNameField is the cache index that stands in for the spec.nodeName
// field selector of the per-node Pod list. It must be registered on the cache
// (IndexField) before the Pod informer starts.
const podNodeNameField = "spec.nodeName"

func podNodeNameIndex(obj client.Object) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}
	return []string{pod.Spec.NodeName}
}

// managerCacheOptions scopes what the controller manager keeps in memory. Pods are the
// risk at cluster scale, so their informer stores only the fields the FSM
// reads; every other kind just drops managedFields.
func managerCacheOptions() cache.Options {
	return cache.Options{
		DefaultTransform: cache.TransformStripManagedFields(),
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}: {Transform: podTransform},
		},
	}
}

// podTransform keeps only what pkg/controller/fsm reads (annotations,
// spec.nodeSelector, spec.affinity, deletionTimestamp, status.phase), the
// spec.nodeName index key, and the identity and ownership metadata. Containers,
// volumes and container statuses, which dominate a Pod's size, never enter the
// cache.
func podTransform(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		// Tombstones (cache.DeletedFinalStateUnknown) wrap an object that was
		// already transformed on its way in.
		return obj, nil
	}
	return &corev1.Pod{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:              pod.Name,
			Namespace:         pod.Namespace,
			UID:               pod.UID,
			ResourceVersion:   pod.ResourceVersion,
			Labels:            pod.Labels,
			Annotations:       pod.Annotations,
			DeletionTimestamp: pod.DeletionTimestamp,
			OwnerReferences:   pod.OwnerReferences,
		},
		Spec: corev1.PodSpec{
			NodeName:     pod.Spec.NodeName,
			NodeSelector: pod.Spec.NodeSelector,
			Affinity:     pod.Spec.Affinity,
		},
		Status: corev1.PodStatus{Phase: pod.Status.Phase},
	}, nil
}

// twinHardwareFromNodeHardware copies the inventory fields the twin model
// reads out of a NodeHardware object into the in-memory struct pkg/controller/twin
// consumes.
func twinHardwareFromNodeHardware(nodeName string, obj *v1alpha1.NodeHardware) joulie.NodeHardware {
	hw := joulie.NodeHardware{NodeName: nodeName}
	if cpu := obj.Status.CPU; cpu != nil {
		hw.CPU.Vendor = cpu.Vendor
		hw.CPU.Model = cpu.Model
		hw.CPU.RawModel = cpu.RawModel
		hw.CPU.Sockets = cpu.Sockets
		hw.CPU.TotalCores = cpu.TotalCores
		hw.CPU.DriverFamily = cpu.DriverFamily
		if cr := cpu.CapRange; cr != nil {
			hw.CPU.CapRange.MaxWattsPerSocket = cr.MaxWattsPerSocket
			hw.CPU.CapRange.MinWattsPerSocket = cr.MinWattsPerSocket
		}
	}
	if gpu := obj.Status.GPU; gpu != nil {
		hw.GPU.Present = gpu.Present
		hw.GPU.Vendor = gpu.Vendor
		hw.GPU.Model = gpu.Model
		hw.GPU.RawModel = gpu.RawModel
		hw.GPU.Count = gpu.Count
		if cr := gpu.CapRangePerGPU; cr != nil {
			hw.GPU.CapRange.MaxWatts = cr.MaxWatts
			hw.GPU.CapRange.MinWatts = cr.MinWatts
		}
		if gpu.Slicing != nil {
			hw.GPU.Slicing.Supported = gpu.Slicing.Supported
		}
	}
	return hw
}
