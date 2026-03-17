package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var (
	reschedulerEvictions = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "joulie_rescheduler_evictions_total",
			Help: "Total pod evictions triggered by the active rescheduler.",
		},
		[]string{"node", "namespace", "reason"},
	)
	reschedulerSkipped = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "joulie_rescheduler_skipped_total",
			Help: "Pods skipped by rescheduler (not reschedulable or rate limited).",
		},
		[]string{"node", "reason"},
	)
)

func init() {
	prometheus.MustRegister(reschedulerEvictions, reschedulerSkipped)
}

type reschedulerConfig struct {
	enabled          bool
	interval         time.Duration
	maxEvictionsPerNode int
	dryRun           bool
}

// reschedulerLoop runs the active rescheduling controller.
//
// It reads NodeTwin.status.rescheduleRecommendations and evicts pods that:
//  1. Have the joulie.io/reschedulable=true annotation
//  2. Are recommended for rescheduling by the twin (stress/draining)
//
// Active rescheduling is disabled by default (ENABLE_ACTIVE_RESCHEDULING=false).
// When enabled, it uses the Kubernetes Eviction API for graceful pod termination.
// kube-scheduler will re-place the pod on a better node using the extender.
//
// Rate limiting: at most maxEvictionsPerNode evictions per node per cycle.
func reschedulerLoop(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, cfg reschedulerConfig) {
	if !cfg.enabled {
		return
	}
	log.Printf("[rescheduler] started: interval=%s maxEvictionsPerNode=%d dryRun=%v",
		cfg.interval, cfg.maxEvictionsPerNode, cfg.dryRun)

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := runRescheduleCycle(ctx, kube, dyn, cfg); err != nil {
				log.Printf("[rescheduler] error: %v", err)
			}
		}
	}
}

func runRescheduleCycle(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, cfg reschedulerConfig) error {
	// List all NodeTwin CRs.
	twins, err := dyn.Resource(nodeTwinGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list NodeTwins: %w", err)
	}

	for _, twin := range twins.Items {
		nodeName := twin.GetName()
		schedulableClass := extractSchedulableClass(twin.Object)
		recs := extractRescheduleRecommendations(twin.Object)
		if len(recs) == 0 {
			continue
		}

		evicted := 0
		for _, rec := range recs {
			if evicted >= cfg.maxEvictionsPerNode {
				reschedulerSkipped.WithLabelValues(nodeName, "rate_limited").Inc()
				break
			}

			ns := rec.namespace
			podName := rec.podName
			reason := rec.reason

			if ns == "" || podName == "" {
				continue
			}

			// Verify pod exists and has reschedulable annotation.
			pod, err := kube.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				continue
			}
			if pod.Annotations["joulie.io/reschedulable"] != "true" {
				reschedulerSkipped.WithLabelValues(nodeName, "not_reschedulable").Inc()
				continue
			}
			// Skip pods not on this node.
			if pod.Spec.NodeName != nodeName {
				continue
			}

			if cfg.dryRun {
				log.Printf("[rescheduler] DRY-RUN: would evict %s/%s from %s (%s)", ns, podName, nodeName, reason)
				continue
			}

			// Annotate the pod's owner (ReplicaSet/StatefulSet) with eviction
			// context so the scheduler avoids re-placing the replacement pod
			// on the same class of node.
			annotateOwnerWithEviction(ctx, kube, ns, pod.OwnerReferences, schedulableClass, reason)

			log.Printf("[rescheduler] evicting %s/%s from %s (%s, class=%s)", ns, podName, nodeName, reason, schedulableClass)
			if err := evictPod(ctx, kube, ns, podName); err != nil {
				log.Printf("[rescheduler] evict %s/%s failed: %v", ns, podName, err)
				continue
			}
			reschedulerEvictions.WithLabelValues(nodeName, ns, reason).Inc()
			evicted++
		}
	}
	return nil
}

type rescheduleRec struct {
	namespace string
	podName   string
	reason    string
}

func extractRescheduleRecommendations(obj map[string]interface{}) []rescheduleRec {
	status, _, _ := unstructured.NestedSlice(obj, "status", "rescheduleRecommendations")
	var recs []rescheduleRec
	for _, item := range status {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		ref, ok := m["workloadRef"].(map[string]interface{})
		if !ok {
			continue
		}
		ns, _ := ref["namespace"].(string)
		name, _ := ref["name"].(string)
		reason, _ := m["reason"].(string)
		recs = append(recs, rescheduleRec{namespace: ns, podName: name, reason: reason})
	}
	return recs
}

// evictPod uses the Kubernetes Eviction API for graceful pod termination.
func evictPod(ctx context.Context, kube kubernetes.Interface, namespace, name string) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return kube.CoreV1().Pods(namespace).EvictV1(ctx, eviction)
}

// cleanupOrphanedWorkloadProfiles removes WorkloadProfile CRs for pods that no longer exist.
func cleanupOrphanedWorkloadProfiles(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface) error {
	profiles, err := dyn.Resource(workloadProfileGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list WorkloadProfiles: %w", err)
	}

	for _, p := range profiles.Items {
		labels := p.GetLabels()
		podName := labels["joulie.io/pod-name"]
		ns := p.GetNamespace()
		if podName == "" || ns == "" {
			continue
		}

		_, err := kube.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			// Pod gone, clean up the profile.
			if err := dyn.Resource(workloadProfileGVR).Namespace(ns).Delete(ctx, p.GetName(), metav1.DeleteOptions{}); err != nil {
				log.Printf("[rescheduler] cleanup WorkloadProfile %s/%s: %v", ns, p.GetName(), err)
			}
		}
	}
	return nil
}

// extractSchedulableClass returns the schedulableClass from a NodeTwin status.
func extractSchedulableClass(obj map[string]interface{}) string {
	v, _, _ := unstructured.NestedString(obj, "status", "schedulableClass")
	return v
}

// annotateOwnerWithEviction patches the pod's owner (ReplicaSet, StatefulSet,
// or Job) with eviction context annotations. The scheduler reads these
// annotations when placing the replacement pod and avoids re-placing it on the
// same class of node that triggered the eviction.
func annotateOwnerWithEviction(ctx context.Context, kube kubernetes.Interface, ns string, ownerRefs []metav1.OwnerReference, fromClass, reason string) {
	if len(ownerRefs) == 0 {
		return
	}
	owner := ownerRefs[0]
	now := time.Now().UTC().Format(time.RFC3339)
	patchData := fmt.Sprintf(
		`{"metadata":{"annotations":{"joulie.io/last-eviction-from-class":%q,"joulie.io/last-eviction-reason":%q,"joulie.io/last-eviction-time":%q}}}`,
		fromClass, reason, now,
	)
	patchBytes := []byte(patchData)

	var err error
	switch owner.Kind {
	case "ReplicaSet":
		_, err = kube.AppsV1().ReplicaSets(ns).Patch(ctx, owner.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	case "StatefulSet":
		_, err = kube.AppsV1().StatefulSets(ns).Patch(ctx, owner.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	case "Job":
		_, err = kube.BatchV1().Jobs(ns).Patch(ctx, owner.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	default:
		log.Printf("[rescheduler] unsupported owner kind %s for eviction annotation; skipping", owner.Kind)
		return
	}
	if err != nil {
		log.Printf("[rescheduler] failed to annotate %s %s/%s with eviction context: %v", owner.Kind, ns, owner.Name, err)
	} else {
		log.Printf("[rescheduler] annotated %s %s/%s: evicted-from-class=%s reason=%s", owner.Kind, ns, owner.Name, fromClass, reason)
	}
}

// workloadProfileStatusToJSON serializes a WorkloadProfile status for logging.
func workloadProfileStatusToJSON(obj map[string]interface{}) string {
	b, _ := json.MarshalIndent(obj, "", "  ")
	return string(b)
}
