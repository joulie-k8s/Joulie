package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNewSchemeRegistersCoreAndOptions(t *testing.T) {
	called := false
	s, err := NewScheme(func(*runtime.Scheme) error { called = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("scheme option was not applied")
	}
	if !s.Recognizes(corev1.SchemeGroupVersion.WithKind("Node")) {
		t.Fatal("core types not registered")
	}
}

// A fake client is a Reader, which is what reconcile loops take in tests.
func TestFakeClientSatisfiesReader(t *testing.T) {
	s, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	var r Reader = fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
	).Build()
	var node corev1.Node
	if err := r.Get(context.Background(), client.ObjectKey{Name: "n1"}, &node); err != nil {
		t.Fatalf("get via Reader: %v", err)
	}
}
