// Package helpers provides utilities for Joulie integration tests.
package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// JoulieGVRs contains the GVRs for all Joulie CRDs.
var JoulieGVRs = map[string]schema.GroupVersionResource{
	"nodetwins":     {Group: "joulie.io", Version: "v1alpha1", Resource: "nodetwins"},
	"nodehardwares": {Group: "joulie.io", Version: "v1alpha1", Resource: "nodehardwares"},
}

// Clients holds k8s client instances for integration tests.
type Clients struct {
	K8s     *kubernetes.Clientset
	Dynamic dynamic.Interface
}

// NewClients creates Kubernetes clients from the default kubeconfig or in-cluster config.
func NewClients(kubeconfigPath string) (*Clients, error) {
	var cfg *rest.Config
	var err error

	if kubeconfigPath != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}

	k8s, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Clients{K8s: k8s, Dynamic: dyn}, nil
}

// WaitForCRD waits until a CRD is registered and listable.
func WaitForCRD(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, false, func(ctx context.Context) (bool, error) {
		_, err := dyn.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
		if err != nil {
			if apierrors.IsNotFound(err) || isServerNotFoundErr(err) {
				return false, nil
			}
			return false, nil // keep retrying
		}
		return true, nil
	})
}

// WaitForObject waits until a named cluster-scoped or namespaced object exists.
func WaitForObject(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, namespace, name string, timeout time.Duration) (*unstructured.Unstructured, error) {
	var result *unstructured.Unstructured
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, false, func(ctx context.Context) (bool, error) {
		var obj *unstructured.Unstructured
		var err error
		if namespace == "" {
			obj, err = dyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
		} else {
			obj, err = dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		}
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, nil
		}
		result = obj
		return true, nil
	})
	return result, err
}

// WaitForStatusField waits until a status field has a non-empty value.
func WaitForStatusField(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, namespace, name string, fieldPath []string, timeout time.Duration) (interface{}, error) {
	var result interface{}
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, false, func(ctx context.Context) (bool, error) {
		var obj *unstructured.Unstructured
		var err error
		if namespace == "" {
			obj, err = dyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
		} else {
			obj, err = dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		}
		if err != nil {
			return false, nil
		}
		v, found, err := unstructured.NestedFieldNoCopy(obj.Object, fieldPath...)
		if err != nil || !found || v == nil || v == "" {
			return false, nil
		}
		result = v
		return true, nil
	})
	return result, err
}

// ApplyUnstructured creates or updates an unstructured object.
func ApplyUnstructured(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) error {
	name := obj.GetName()
	var err error
	if namespace == "" {
		_, err = dyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	} else {
		_, err = dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	}

	if apierrors.IsNotFound(err) {
		if namespace == "" {
			_, err = dyn.Resource(gvr).Create(ctx, obj, metav1.CreateOptions{})
		} else {
			_, err = dyn.Resource(gvr).Namespace(namespace).Create(ctx, obj, metav1.CreateOptions{})
		}
	} else if err == nil {
		if namespace == "" {
			_, err = dyn.Resource(gvr).Update(ctx, obj, metav1.UpdateOptions{})
		} else {
			_, err = dyn.Resource(gvr).Namespace(namespace).Update(ctx, obj, metav1.UpdateOptions{})
		}
	}
	return err
}

// MustParseCR parses a CR from JSON string and panics on error.
func MustParseCR(t *testing.T, jsonStr string) *unstructured.Unstructured {
	t.Helper()
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &obj); err != nil {
		t.Fatalf("parse CR: %v", err)
	}
	return &unstructured.Unstructured{Object: obj}
}

func isServerNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	return apierrors.IsNotFound(err) ||
		apierrors.IsMethodNotSupported(err) ||
		apierrors.IsServiceUnavailable(err)
}

// AssertNodeLabel checks that a node has the expected label value.
func AssertNodeLabel(ctx context.Context, t *testing.T, k8s *kubernetes.Clientset, nodeName, labelKey, expectedValue string) {
	t.Helper()
	node, err := k8s.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node %s: %v", nodeName, err)
	}
	val := node.Labels[labelKey]
	if val != expectedValue {
		t.Errorf("node %s label %s = %q, want %q", nodeName, labelKey, val, expectedValue)
	}
}
