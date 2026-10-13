package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
