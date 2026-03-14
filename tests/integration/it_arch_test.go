//go:build integration

package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/matbun/joulie/tests/integration/helpers"
)

// IT-ARCH-01: Operator, agent, and scheduler plugin install smoke test.
// Asserts: CRDs registered, critical objects listable.
func TestIT_ARCH_01_CRDsRegistered(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	clients, err := helpers.NewClients(kubeconfig)
	if err != nil {
		t.Skipf("no kubeconfig available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for name, gvr := range helpers.JoulieGVRs {
		t.Run(name, func(t *testing.T) {
			if err := helpers.WaitForCRD(ctx, clients.Dynamic, gvr, 30*time.Second); err != nil {
				t.Errorf("CRD %s not registered within 30s: %v", name, err)
			}
		})
	}
}
