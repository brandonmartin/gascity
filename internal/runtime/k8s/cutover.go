package k8s

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing SleepCapability
// through to the underlying *Provider. The early cut-over for the k8s provider.
//
// ExecProvider is not passed through: k8s's Exec (execInPod) is the connection
// the carrier drives over internally; no production caller type-asserts it.
type seamBackedProvider struct {
	runtime.Provider
	raw *Provider
}

var (
	_ runtime.Provider                = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider = (*seamBackedProvider)(nil)
	_ runtime.RelaunchProvider        = (*seamBackedProvider)(nil)
)

// SeamOptions carries the pod-shape configuration for the k8s provider that is
// independent of the wire adapter (k8sOps). Production derives it from the
// process environment via [seamOptionsFromEnv]; tests supply it directly.
//
// Zero-value fields fall back to the same defaults NewProvider uses so a bare
// SeamOptions{Image: "..."} is enough to exercise the seam contract.
type SeamOptions struct {
	Namespace          string
	Image              string
	Context            string
	ServiceAccount     string
	ManagedServiceHost string
	ManagedServicePort string
	CPURequest         string
	MemRequest         string
	CPULimit           string
	MemLimit           string
	Prebaked           bool
	NodeSelector       map[string]string
	Tolerations        []corev1.Toleration
	Affinity           *corev1.Affinity
	PriorityClassName  string
	Stderr             io.Writer
	PostStartSettle    time.Duration
}

// NewSeamBackedWithOps composes a k8sOps seam and a SeamOptions shape into a
// seam-backed [runtime.Provider]. Every kube-apiserver call flows through the
// supplied ops, so the shared runtime.Provider contract can be proved end-to-end
// against a fake ops double without touching a live cluster.
//
// Zero-value option fields keep the same defaults NewProvider uses. This is a
// pure composition: no environment reads, no adapter construction, no error
// path — those live in the production wire (NewSeamBacked → newRealAdapter,
// seamOptionsFromEnv), which folds any of their failures into its own error
// return before calling into here.
func NewSeamBackedWithOps(ops k8sOps, opts SeamOptions) runtime.Provider {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	postStartSettle := opts.PostStartSettle
	if postStartSettle == 0 {
		postStartSettle = 3 * time.Second
	}
	raw := &Provider{
		ops:                ops,
		namespace:          firstNonEmpty(opts.Namespace, "gc"),
		image:              opts.Image,
		k8sContext:         opts.Context,
		managedServiceHost: firstNonEmpty(opts.ManagedServiceHost, podManagedDoltHost),
		managedServicePort: firstNonEmpty(opts.ManagedServicePort, podManagedDoltPort),
		cpuRequest:         firstNonEmpty(opts.CPURequest, "500m"),
		memRequest:         firstNonEmpty(opts.MemRequest, "1Gi"),
		cpuLimit:           firstNonEmpty(opts.CPULimit, "2"),
		memLimit:           firstNonEmpty(opts.MemLimit, "4Gi"),
		serviceAccount:     opts.ServiceAccount,
		prebaked:           opts.Prebaked,
		postStartSettle:    postStartSettle,
		stderr:             stderr,
		nodeSelector:       opts.NodeSelector,
		tolerations:        opts.Tolerations,
		affinity:           opts.Affinity,
		priorityClassName:  opts.PriorityClassName,
	}
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: runtime.NewProviderFromSeams(rt, tp), raw: raw}
}

// NewSeamBacked constructs a k8s provider served through the seams — thin
// composition over [NewSeamBackedWithOps] retained as a convenience for
// callers that want the full production wiring in one step. cmd/gc's runtime
// registry composes the pieces itself so the ledger can bind the discovered
// constructor directly to [NewSeamBackedWithOps]; every other caller can keep
// using this shortcut.
func NewSeamBacked() (runtime.Provider, error) {
	ops, err := NewRealAdapter()
	if err != nil {
		return nil, err
	}
	opts, err := SeamOptionsFromEnv()
	if err != nil {
		return nil, err
	}
	return NewSeamBackedWithOps(ops, opts), nil
}

// NewRealAdapter builds the production k8sOps adapter: the client-go
// clientset against the resolved REST config, and the namespace the adapter
// targets. Every kube-apiserver call in production flows through the returned
// value. Exported so cmd/gc's runtime registry can compose it with
// [NewSeamBackedWithOps] directly — the ledger reads that composition as the
// production constructor.
func NewRealAdapter() (Ops, error) {
	namespace := envOrDefault("GC_K8S_NAMESPACE", "gc")
	k8sContext := os.Getenv("GC_K8S_CONTEXT")
	restConfig, err := buildRESTConfig(k8sContext)
	if err != nil {
		return nil, fmt.Errorf("building K8s config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating K8s clientset: %w", err)
	}
	return &realK8sOps{clientset: clientset, restConfig: restConfig, namespace: namespace}, nil
}

// SeamOptionsFromEnv reads the same process environment NewProvider used and
// packages it into a [SeamOptions] for [NewSeamBackedWithOps]. Split from
// NewRealAdapter so failures in user-supplied JSON envs (scheduling,
// managed-service alias) surface without needing a live cluster.
func SeamOptionsFromEnv() (SeamOptions, error) {
	scheduling, err := parseSchedulingEnv()
	if err != nil {
		return SeamOptions{}, err
	}
	managedHost, managedPort, err := managedServiceAlias()
	if err != nil {
		return SeamOptions{}, err
	}
	return SeamOptions{
		Namespace:          envOrDefault("GC_K8S_NAMESPACE", "gc"),
		Image:              os.Getenv("GC_K8S_IMAGE"),
		Context:            os.Getenv("GC_K8S_CONTEXT"),
		ServiceAccount:     os.Getenv("GC_K8S_SERVICE_ACCOUNT"),
		ManagedServiceHost: managedHost,
		ManagedServicePort: managedPort,
		CPURequest:         envOrDefault("GC_K8S_CPU_REQUEST", "500m"),
		MemRequest:         envOrDefault("GC_K8S_MEM_REQUEST", "1Gi"),
		CPULimit:           envOrDefault("GC_K8S_CPU_LIMIT", "2"),
		MemLimit:           envOrDefault("GC_K8S_MEM_LIMIT", "4Gi"),
		Prebaked:           os.Getenv("GC_K8S_PREBAKED") == "true",
		NodeSelector:       scheduling.nodeSelector,
		Tolerations:        scheduling.tolerations,
		Affinity:           scheduling.affinity,
		PriorityClassName:  scheduling.priorityClassName,
	}, nil
}

func firstNonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// SleepCapability passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	return s.raw.SleepCapability(name)
}

// Relaunch passes through to the underlying provider's warm-pod relaunch
// (respawn-pane via execInPod; B2, RelaunchProvider).
func (s *seamBackedProvider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	return s.raw.Relaunch(ctx, name, cfg)
}
