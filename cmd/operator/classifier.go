package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/workloadprofile/classifier"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// classifierLoop runs the workload classifier on a regular interval.
// It watches running pods on managed nodes, classifies them using
// Kepler/cAdvisor metrics from Prometheus, and writes WorkloadProfile CRs.
//
// This closes the feedback loop: classifier writes WorkloadProfiles,
// operator reads them in twinstate.go for twin computation, scheduler
// uses the twin output for energy-aware placement.
func classifierLoop(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, cfg classifierConfig) {
	cl := classifier.NewClassifier(classifier.ClassifierConfig{
		MetricsWindow:      cfg.metricsWindow,
		ReclassifyInterval: cfg.reclassifyInterval,
		Prometheus: classifier.PrometheusConfig{
			Address:         cfg.prometheusAddress,
			Timeout:         5 * time.Second,
			KeplerAvailable: cfg.keplerAvailable,
		},
		MinConfidence: cfg.minConfidence,
	})

	// Track last classification time per pod to avoid reclassifying too often.
	lastClassified := make(map[string]time.Time)

	log.Printf("[classifier] started: interval=%s prometheus=%s kepler=%v",
		cfg.classifyInterval, cfg.prometheusAddress, cfg.keplerAvailable)

	ticker := time.NewTicker(cfg.classifyInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := classifyAllPods(ctx, kube, dyn, cl, cfg, lastClassified); err != nil {
				log.Printf("[classifier] error: %v", err)
			}
		}
	}
}

type classifierConfig struct {
	classifyInterval     time.Duration
	reclassifyInterval   time.Duration
	metricsWindow        time.Duration
	prometheusAddress    string
	keplerAvailable      bool
	minConfidence        float64
	nodeSelector         string
}

func classifyAllPods(
	ctx context.Context,
	kube kubernetes.Interface,
	dyn dynamic.Interface,
	cl *classifier.Classifier,
	cfg classifierConfig,
	lastClassified map[string]time.Time,
) error {
	// List all running pods across all namespaces.
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}

	// Get managed nodes.
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	managedNodes := make(map[string]bool)
	for _, n := range nodes.Items {
		if n.Labels["joulie.io/managed"] == "true" {
			managedNodes[n.Name] = true
		}
	}

	classified := 0
	for _, pod := range pods.Items {
		// Skip pods not on managed nodes.
		if !managedNodes[pod.Spec.NodeName] {
			continue
		}
		// Skip system namespaces.
		ns := pod.Namespace
		if ns == "kube-system" || ns == "kube-public" || ns == "kube-node-lease" || ns == "joulie-system" {
			continue
		}

		key := ns + "/" + pod.Name
		if last, ok := lastClassified[key]; ok {
			if time.Since(last) < cfg.reclassifyInterval {
				continue
			}
		}

		wp, err := cl.ClassifyPod(ctx, ns, pod.Name, pod.Labels, pod.Annotations)
		if err != nil {
			log.Printf("[classifier] classify %s: %v", key, err)
			continue
		}

		if err := upsertWorkloadProfile(ctx, dyn, ns, pod.Name, pod.Spec.NodeName, wp); err != nil {
			log.Printf("[classifier] upsert WorkloadProfile %s: %v", key, err)
			continue
		}

		lastClassified[key] = time.Now()
		classified++
	}

	// Clean up entries for pods that no longer exist.
	activePods := make(map[string]bool)
	for _, pod := range pods.Items {
		activePods[pod.Namespace+"/"+pod.Name] = true
	}
	for key := range lastClassified {
		if !activePods[key] {
			delete(lastClassified, key)
		}
	}

	if classified > 0 {
		log.Printf("[classifier] classified %d pods", classified)
	}
	return nil
}

// upsertWorkloadProfile creates or updates a WorkloadProfile CR for a pod.
func upsertWorkloadProfile(
	ctx context.Context,
	dyn dynamic.Interface,
	namespace, podName, nodeName string,
	wp joulie.WorkloadProfileStatus,
) error {
	name := fmt.Sprintf("%s-%s", namespace, podName)
	// Truncate to 253 chars (k8s name limit).
	if len(name) > 253 {
		name = name[:253]
	}

	statusMap := workloadProfileStatusToMap(wp)

	// Try to get existing.
	existing, err := dyn.Resource(workloadProfileGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get WorkloadProfile %s: %w", name, err)
	}

	if apierrors.IsNotFound(err) {
		// Create new.
		obj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "joulie.io/v1alpha1",
				"kind":       "WorkloadProfile",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
					"labels": map[string]interface{}{
						"joulie.io/pod-name":  podName,
						"joulie.io/node-name": nodeName,
					},
				},
				"spec": map[string]interface{}{
					"nodeName": nodeName,
					"workloadRef": map[string]interface{}{
						"kind":      "Pod",
						"namespace": namespace,
						"name":      podName,
					},
				},
			},
		}
		created, err := dyn.Resource(workloadProfileGVR).Namespace(namespace).Create(ctx, obj, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create WorkloadProfile %s: %w", name, err)
		}

		// Patch status subresource.
		patch := map[string]interface{}{"status": statusMap}
		patchBytes, _ := json.Marshal(patch)
		_, err = dyn.Resource(workloadProfileGVR).Namespace(namespace).Patch(
			ctx, created.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
		)
		if err != nil {
			// Fallback: full patch.
			fullPatch := map[string]interface{}{
				"apiVersion": "joulie.io/v1alpha1",
				"kind":       "WorkloadProfile",
				"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
				"status":     statusMap,
			}
			fp, _ := json.Marshal(fullPatch)
			_, err = dyn.Resource(workloadProfileGVR).Namespace(namespace).Patch(
				ctx, name, types.MergePatchType, fp, metav1.PatchOptions{},
			)
			if err != nil {
				return fmt.Errorf("patch WorkloadProfile %s status: %w", name, err)
			}
		}
		return nil
	}

	// Update existing: update spec.nodeName and status.
	specMap, _, _ := unstructured.NestedMap(existing.Object, "spec")
	if specMap == nil {
		specMap = map[string]interface{}{}
	}
	specMap["nodeName"] = nodeName
	existing.Object["spec"] = specMap

	// Update labels.
	labels := existing.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["joulie.io/pod-name"] = podName
	labels["joulie.io/node-name"] = nodeName
	existing.SetLabels(labels)

	if _, err := dyn.Resource(workloadProfileGVR).Namespace(namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update WorkloadProfile %s: %w", name, err)
	}

	// Patch status.
	patch := map[string]interface{}{"status": statusMap}
	patchBytes, _ := json.Marshal(patch)
	_, err = dyn.Resource(workloadProfileGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	if err != nil {
		fullPatch := map[string]interface{}{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "WorkloadProfile",
			"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
			"status":     statusMap,
		}
		fp, _ := json.Marshal(fullPatch)
		_, err = dyn.Resource(workloadProfileGVR).Namespace(namespace).Patch(
			ctx, name, types.MergePatchType, fp, metav1.PatchOptions{},
		)
		if err != nil {
			return fmt.Errorf("patch WorkloadProfile %s status: %w", name, err)
		}
	}
	return nil
}

func workloadProfileStatusToMap(wp joulie.WorkloadProfileStatus) map[string]interface{} {
	m := map[string]interface{}{
		"criticality": map[string]interface{}{
			"class": wp.Criticality.Class,
		},
		"migratability": map[string]interface{}{
			"reschedulable": wp.Migratability.Reschedulable,
		},
		"cpu": map[string]interface{}{
			"intensity":         wp.CPU.Intensity,
			"bound":             wp.CPU.Bound,
			"avgUtilizationPct": wp.CPU.AvgUtilizationPct,
			"capSensitivity":    wp.CPU.CapSensitivity,
		},
		"gpu": map[string]interface{}{
			"intensity":         wp.GPU.Intensity,
			"bound":             wp.GPU.Bound,
			"avgUtilizationPct": wp.GPU.AvgUtilizationPct,
			"capSensitivity":    wp.GPU.CapSensitivity,
		},
		"classificationReason": wp.ClassificationReason,
		"confidence":           wp.Confidence,
		"lastUpdated":          wp.LastUpdated.Format(time.RFC3339),
	}
	if wp.RescheduleRecommended {
		m["rescheduleRecommended"] = true
		m["rescheduleReason"] = wp.RescheduleReason
	}
	return m
}
