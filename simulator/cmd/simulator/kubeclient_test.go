package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/matbun/joulie/api/v1alpha1"
)

// kubeTestConfig is a kubeconfig with one cluster at server.
const kubeTestConfig = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster: {server: "https://kube.example.test:6443"}
users:
- name: test
  user: {token: not-a-secret}
contexts:
- name: test
  context: {cluster: test, user: test}
current-context: test
`

// kubeTestEnv isolates newKubeConfig from the machine: a temporary HOME
// and no in-cluster service variables.
func kubeTestEnv(t *testing.T, kubeconfig string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("KUBECONFIG", kubeconfig)
}

func TestSE10NewKubeConfigReadsKUBECONFIG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(kubeTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	kubeTestEnv(t, path)
	cfg, err := newKubeConfig()
	if err != nil {
		t.Fatalf("newKubeConfig: %v", err)
	}
	if cfg.Host != "https://kube.example.test:6443" {
		t.Errorf("Host %q, want the kubeconfig's server", cfg.Host)
	}
	if _, err := newKubeClient(20, 40, 0); err != nil {
		t.Errorf("newKubeClient: %v", err)
	}
}

func TestSE10NewKubeConfigNamesBothSourcesWhenNeitherWorks(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-kubeconfig")
	kubeTestEnv(t, missing)
	cfg, err := newKubeConfig()
	if err == nil {
		t.Fatalf("newKubeConfig with no kubeconfig and no cluster returned %+v", cfg)
	}
	for _, want := range []string{"KUBECONFIG=" + missing, "in-cluster", "KUBERNETES_SERVICE_HOST"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if _, err := kubeClientFactory(20, 40, 0); err == nil {
		t.Error("kubeClientFactory without a configuration returned a client")
	}
}

// kubeTestServer is an API server that answers in protobuf what has a
// protobuf encoding, as a real one does for a client that accepts it: the
// pod list and the discovery of joulie.io/v1alpha1. The NodeTwin list, a
// custom resource, is JSON. It records the Accept header of each path.
func kubeTestServer(t *testing.T) (*httptest.Server, func(path string) string) {
	t.Helper()
	proto := func(obj runtime.Object, gv schema.GroupVersion) []byte {
		info, ok := runtime.SerializerInfoForMediaType(scheme.Codecs.SupportedMediaTypes(), runtime.ContentTypeProtobuf)
		if !ok {
			t.Fatal("no protobuf serializer")
		}
		b, err := runtime.Encode(scheme.Codecs.EncoderForVersion(info.Serializer, gv), obj)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	pods := &corev1.PodList{Items: []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p1"},
		Spec:       corev1.PodSpec{NodeName: "n1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}}}
	resources := &metav1.APIResourceList{GroupVersion: v1alpha1.GroupVersion.String(), APIResources: []metav1.APIResource{{Name: "nodetwins", Kind: "NodeTwin", Namespaced: false}}}
	var mu sync.Mutex
	accept := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		accept[r.URL.Path] = r.Header.Get("Accept")
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/pods":
			w.Header().Set("Content-Type", runtime.ContentTypeProtobuf)
			_, _ = w.Write(proto(pods, corev1.SchemeGroupVersion))
		case "/apis/" + v1alpha1.GroupVersion.String():
			w.Header().Set("Content-Type", runtime.ContentTypeProtobuf)
			_, _ = w.Write(proto(resources, metav1.SchemeGroupVersion))
		case "/apis/" + v1alpha1.GroupVersion.String() + "/nodetwins":
			w.Header().Set("Content-Type", runtime.ContentTypeJSON)
			_, _ = w.Write([]byte(`{"apiVersion":"joulie.io/v1alpha1","kind":"NodeTwinList","items":[{"metadata":{"name":"n1"},"spec":{"nodeName":"n1"},"status":{"schedulableClass":"eco"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func(path string) string {
		mu.Lock()
		defer mu.Unlock()
		return accept[path]
	}
}

// The typed clientset asks for protobuf, with JSON as the fallback, and
// reads both: the pod poll decodes a protobuf pod list, and the NodeTwin
// poll a protobuf discovery answer and a JSON custom resource list.
func TestSE10TypedClientSpeaksProtobuf(t *testing.T) {
	srv, accept := kubeTestServer(t)
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(strings.Replace(kubeTestConfig, "https://kube.example.test:6443", srv.URL, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	kubeTestEnv(t, path)
	kube, err := newKubeClient(20, 40, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	counts, details, err := listPodsForPoll(context.Background(), kube)
	if err != nil || counts["n1"] != 1 || len(details) != 1 {
		t.Fatalf("pod poll over protobuf: counts %v, %d details, %v; want n1 with one Running pod", counts, len(details), err)
	}
	twins, err := listNodeTwinsREST(kube)(context.Background())
	if err != nil || len(twins) != 1 || twins[0].Status.SchedulableClass != "eco" {
		t.Fatalf("NodeTwins: %+v, %v; want n1 with class eco", twins, err)
	}
	for _, p := range []string{"/api/v1/pods", "/apis/" + v1alpha1.GroupVersion.String() + "/nodetwins"} {
		if got := accept(p); got != kubeProtobufAccept {
			t.Errorf("Accept of %s = %q, want %q", p, got, kubeProtobufAccept)
		}
	}
}
