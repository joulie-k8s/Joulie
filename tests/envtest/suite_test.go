//go:build envtest

// Package envtest runs the Joulie CRDs against a real Kubernetes API server
// (etcd plus kube-apiserver, started by sigs.k8s.io/controller-runtime/pkg/envtest).
//
// Everything else in the repository proves schema and ownership indirectly: a
// unit test compares the generated enum against pkg/api, and the ownership
// rules are checked by inspecting the shape of the patch payloads the
// components build. Neither can fail the way production fails, because
// neither runs the code that validates and merges the writes: the API server.
// This suite does, so a drifted enum or a field manager reaching outside its
// subtree is caught here instead of on a cluster.
//
// The suite sits behind the `envtest` build tag because it needs downloaded
// control-plane binaries; `go test ./...` stays fast and hermetic.
package envtest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/kube"
)

const skipMessage = "envtest needs control-plane binaries: run `make test-envtest`, " +
	"or set KUBEBUILDER_ASSETS to the output of `setup-envtest use 1.31.0 -p path`"

var (
	// skipReason is non-empty only when KUBEBUILDER_ASSETS is unset. Once a
	// control plane is available every failure is a real failure.
	skipReason string
	testEnv    *envtest.Environment
	restConfig *rest.Config
	scheme     *runtime.Scheme
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		skipReason = skipMessage
		os.Exit(m.Run())
	}

	kube.UseStdLogger()

	var err error
	scheme, err = kube.NewScheme(v1alpha1.AddToScheme)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build scheme: %v\n", err)
		os.Exit(1)
	}

	// CRDDirectoryPaths points at the generated CRDs, not at a copy written
	// for the test, so the API server validates exactly what
	// `make manifests` produces and what a cluster installs.
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	restConfig, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest control plane: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest control plane: %v\n", err)
	}
	os.Exit(code)
}

// newClient returns a client talking to the envtest API server, or skips the
// test when no control plane was started.
func newClient(t *testing.T) client.Client {
	t.Helper()
	if skipReason != "" {
		t.Skip(skipReason)
	}
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return c
}

// testContext gives every test the same bounded lifetime, so a hung API call
// fails one test instead of timing out the package.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// objectName derives an object name from the test name. NodeTwin and
// NodeHardware are cluster scoped, so tests would otherwise collide on the
// shared API server.
func objectName(t *testing.T, suffix string) string {
	t.Helper()
	name := "envtest-" + sanitize(t.Name())
	if suffix != "" {
		name += "-" + sanitize(suffix)
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func sanitize(in string) string {
	out := make([]rune, 0, len(in))
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
