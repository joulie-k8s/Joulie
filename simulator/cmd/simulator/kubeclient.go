package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// kubeClientFactory builds the simulator's Kubernetes clients, with the
// client-side rate limit and request timeout of each loop; tests replace it.
var kubeClientFactory = newKubeClient

// newKubeConfig finds the cluster the simulator talks to as kubectl does:
// KUBECONFIG, else ~/.kube/config, and in a pod without either the
// in-cluster configuration (client-go's deferred loading with its
// in-cluster fallback), so the simulator also runs from a workstation. It
// logs the API server it uses. When neither source gives a configuration,
// the error names both.
func newKubeConfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		if clientcmd.IsEmptyConfig(err) {
			return nil, fmt.Errorf("no Kubernetes configuration: no kubeconfig at %s, and no in-cluster configuration (KUBERNETES_SERVICE_HOST, KUBERNETES_SERVICE_PORT or the service account token is missing)",
				kubeconfigSources(rules))
		}
		return nil, fmt.Errorf("kubeconfig at %s: %w", kubeconfigSources(rules), err)
	}
	log.Printf("Kubernetes API server %s", cfg.Host)
	return cfg, nil
}

// kubeconfigSources names the kubeconfig files the rules read.
func kubeconfigSources(rules *clientcmd.ClientConfigLoadingRules) string {
	files := strings.Join(rules.GetLoadingPrecedence(), ", ")
	if os.Getenv(clientcmd.RecommendedConfigPathEnvVar) != "" {
		return clientcmd.RecommendedConfigPathEnvVar + "=" + files
	}
	return files
}

// newKubeClient is a clientset over newKubeConfig with qps and burst as its
// client-side rate limit and timeout per request.
func newKubeClient(qps float32, burst int, timeout time.Duration) (kubernetes.Interface, error) {
	cfg, err := newKubeConfig()
	if err != nil {
		return nil, err
	}
	cfg.QPS = qps
	cfg.Burst = burst
	cfg.Timeout = timeout
	return kubernetes.NewForConfig(cfg)
}
