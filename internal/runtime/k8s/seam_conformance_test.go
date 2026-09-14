package k8s

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestK8sSeamBackedWithOpsConformance runs the full shared runtime.Provider
// conformance suite against NewSeamBackedWithOps composed with an in-memory
// k8sOps double. Production wiring (NewSeamBacked) is a thin composition of
// NewSeamBackedWithOps over the real client-go adapter, so proving the seam
// contract here proves the composition for every disposition except the real
// adapter itself (kubeconfig / client-go wiring) — that is the residue the
// ledger names.
//
// The double simulates just enough tmux behavior for the shared contract:
//   - pod lifecycle (create/get/list/delete) via fakeK8sOps,
//   - per-pod tmux-environment key/value round-trip for the MetaStore group,
//   - `tmux has-session -t main` succeeds while the pod is present,
//   - every other tmux command (send-keys, capture-pane, pipe-pane,
//     display-message, pgrep, …) succeeds with an empty stdout — that matches
//     the best-effort contract of the observation/signaling groups.
//
// The mutex around fakeK8sOps is required for the concurrent-lifecycle cases
// (Start_ConcurrentDistinctSessions, Stop_ConcurrentDistinctSessions, …); the
// inner fake is otherwise single-threaded.
func TestK8sSeamBackedWithOpsConformance(t *testing.T) {
	var counter int64

	runtimetest.RunProviderTests(t, func(_ *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBackedWithOps(newSeamConformanceOps(), SeamOptions{Image: "gc-agent:test", Stderr: io.Discard}), runtime.Config{
			Command: "true",
			WorkDir: "",
		}, fmt.Sprintf("gc-k8s-conform-%d-%d", os.Getpid(), atomic.AddInt64(&counter, 1))
	})
}

// conformanceOps wraps fakeK8sOps with a mutex and a per-pod tmux environment,
// so the shared conformance suite (which exercises concurrent lifecycle and the
// metadata round-trip) has a coherent view of pod and tmux state.
type conformanceOps struct {
	mu    sync.Mutex
	inner *fakeK8sOps
	env   map[string]map[string]string
}

// newSeamConformanceOps returns a k8sOps double sufficient for the shared
// runtime.Provider conformance suite. Exported to the ledger's ValidateProofRefs
// as an allowed factory call — every other call inside the proof factory is
// either the constructor under test or a listed built-in.
func newSeamConformanceOps() *conformanceOps {
	return &conformanceOps{
		inner: newFakeK8sOps(),
		env:   map[string]map[string]string{},
	}
}

var _ k8sOps = (*conformanceOps)(nil)

func (o *conformanceOps) createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inner.createPod(ctx, pod)
}

func (o *conformanceOps) getPod(ctx context.Context, name string) (*corev1.Pod, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inner.getPod(ctx, name)
}

func (o *conformanceOps) deletePod(ctx context.Context, name string, grace int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.env, name)
	return o.inner.deletePod(ctx, name, grace)
}

func (o *conformanceOps) listPods(ctx context.Context, selector, fieldSelector string) ([]corev1.Pod, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inner.listPods(ctx, selector, fieldSelector)
}

func (o *conformanceOps) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if out, handled := o.interceptTmux(pod, cmd); handled {
		return out, nil
	}
	return o.inner.execInPod(ctx, pod, container, cmd, stdin)
}

// interceptTmux serves the tmux commands that must round-trip through the fake
// for the shared contract: per-pod set/show/unset-environment (the MetaStore
// group), and has-session (Start's waitForTmux and the stale-probe path). Every
// other tmux command falls through and gets the fake's default empty-stdout
// success, which is the best-effort contract the observation/signaling groups
// already tolerate.
func (o *conformanceOps) interceptTmux(pod string, cmd []string) (string, bool) {
	if len(cmd) < 2 || cmd[0] != "tmux" {
		return "", false
	}
	switch cmd[1] {
	case "has-session":
		// The pod being present means the box is up; the shared contract does
		// not exercise a "pod up but tmux dead" state.
		if _, ok := o.inner.pods[pod]; ok {
			return "", true
		}
		return "", false
	case "set-environment":
		return o.setEnvironment(pod, cmd)
	case "show-environment":
		return o.showEnvironment(pod, cmd)
	}
	return "", false
}

// setEnvironment handles both `tmux set-environment -t main KEY VALUE` (store)
// and `tmux set-environment -t main -u KEY` (explicit unset). The `-t <target>`
// pair is expected before the KEY/VALUE tail; anything shorter falls through.
func (o *conformanceOps) setEnvironment(pod string, cmd []string) (string, bool) {
	args := trimTarget(cmd[2:])
	if len(args) == 0 {
		return "", false
	}
	if args[0] == "-u" && len(args) == 2 {
		if store, ok := o.env[pod]; ok {
			delete(store, args[1])
		}
		return "", true
	}
	if len(args) != 2 {
		return "", false
	}
	if _, ok := o.env[pod]; !ok {
		o.env[pod] = map[string]string{}
	}
	o.env[pod][args[0]] = args[1]
	return "", true
}

func (o *conformanceOps) showEnvironment(pod string, cmd []string) (string, bool) {
	args := trimTarget(cmd[2:])
	if len(args) != 1 {
		return "", false
	}
	key := args[0]
	if store, ok := o.env[pod]; ok {
		if val, present := store[key]; present {
			return key + "=" + val, true
		}
	}
	// tmux prints "-KEY" for an explicitly-unset key; the provider parses that
	// to empty. Same for "not set" — both surface as an empty GetMeta.
	return "-" + key, true
}

// trimTarget strips a leading `-t <target>` pair from the argument tail so
// interceptors can focus on the key/value shape.
func trimTarget(args []string) []string {
	if len(args) >= 2 && args[0] == "-t" {
		return args[2:]
	}
	return args
}
