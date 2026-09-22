// Package kube holds the Kubernetes plumbing shared by the Joulie binaries:
// the runtime scheme and an informer-backed cache for reads.
//
// Reads go through the cache so that a reconcile touches the API server only
// when it writes. The previous design fetched every object on every tick,
// which made the controller manager's API call count proportional to the node count and
// every agent list all NodeTwins to find its own.
package kube

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var useStdLoggerOnce sync.Once

// UseStdLogger routes controller-runtime's logr output to the standard
// logger, so cache and leader election messages carry the binary's prefix.
// Without a logger set, controller-runtime prints a stack trace after 30 s.
func UseStdLogger() {
	useStdLoggerOnce.Do(func() {
		ctrllog.SetLogger(funcr.New(func(prefix, args string) {
			log.Print(strings.TrimSpace(prefix + " " + args))
		}, funcr.Options{}))
	})
}

// SchemeOption registers additional types on the scheme built by NewScheme.
type SchemeOption func(*runtime.Scheme) error

// NewScheme returns a scheme with the core types registered plus any options.
func NewScheme(opts ...SchemeOption) (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		return nil, err
	}
	for _, o := range opts {
		if err := o(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// NewCache builds an informer-backed cache. opts.Scheme must be set; use
// opts.ByObject to restrict what a process watches, for example an agent
// restricting NodeTwin to its own node by metadata.name.
func NewCache(cfg *rest.Config, opts cache.Options) (cache.Cache, error) {
	if opts.Scheme == nil {
		return nil, fmt.Errorf("cache: scheme is required")
	}
	return cache.New(cfg, opts)
}

// Start runs the cache in the background and blocks until its informers have
// synced or ctx is done.
func Start(ctx context.Context, c cache.Cache) error {
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(ctx) }()
	if !c.WaitForCacheSync(ctx) {
		select {
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("cache: %w", err)
			}
		default:
		}
		return fmt.Errorf("cache: informers did not sync before context ended")
	}
	return nil
}

// Reader is the read-only view handed to reconcile loops. A cache satisfies
// it in production; a fake client satisfies it in tests.
type Reader = client.Reader

// ManagerOptions is the subset of manager configuration Joulie exposes.
type ManagerOptions struct {
	Scheme *runtime.Scheme
	// LeaderElect enables leader election on the given lease; when several
	// replicas run, only the leader executes runnables.
	LeaderElect      bool
	LeaderElectionID string
	LeaderElectionNS string
	// Cache is applied to the manager's cache (scoping, transforms).
	Cache cache.Options
}

// NewManager builds a controller-runtime manager with Joulie's defaults: no
// built-in metrics server (each binary already serves its own /metrics) and
// no health probe endpoints.
func NewManager(cfg *rest.Config, opts ManagerOptions) (manager.Manager, error) {
	if opts.Scheme == nil {
		return nil, fmt.Errorf("manager: scheme is required")
	}
	opts.Cache.Scheme = opts.Scheme
	return manager.New(cfg, manager.Options{
		Scheme:                  opts.Scheme,
		Cache:                   opts.Cache,
		LeaderElection:          opts.LeaderElect,
		LeaderElectionID:        opts.LeaderElectionID,
		LeaderElectionNamespace: opts.LeaderElectionNS,
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
	})
}
