package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

type sshConformanceFixture struct {
	once     sync.Once
	binDir   string
	stateDir string
	err      error
}

func TestSSHConformance(t *testing.T) {
	var fixture sshConformanceFixture
	var counter int64

	runtimetest.RunProviderTests(t, func(caseT *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBacked(sshConformanceEndpoint(caseT, t, &fixture)), runtime.Config{
			Command: "sleep 300",
			WorkDir: caseT.TempDir(),
		}, fmt.Sprintf("gcssh%d", atomic.AddInt64(&counter, 1))
	})
}

func sshConformanceEndpoint(caseT, ownerT *testing.T, fixture *sshConformanceFixture) Endpoint {
	caseT.Helper()
	if err := prepareSSHConformanceFixture(ownerT, fixture); err != nil {
		caseT.Fatal(err)
	}
	caseT.Setenv("GC_SSH_CONFORMANCE_STATE", fixture.stateDir)
	path := os.Getenv("PATH")
	prefix := fixture.binDir + string(os.PathListSeparator)
	if !strings.HasPrefix(path, prefix) {
		caseT.Setenv("PATH", prefix+path)
	}
	return Endpoint{Host: "conformance-box"}
}

func prepareSSHConformanceFixture(ownerT *testing.T, fixture *sshConformanceFixture) error {
	fixture.once.Do(func() {
		fixtureRoot, err := os.MkdirTemp("", "gc-ssh-conformance-")
		if err != nil {
			fixture.err = fmt.Errorf("create ssh conformance fixture: %w", err)
			return
		}
		ownerT.Cleanup(func() {
			if err := os.RemoveAll(fixtureRoot); err != nil {
				ownerT.Errorf("remove ssh conformance fixture %q: %v", fixtureRoot, err)
			}
		})

		fixture.binDir = filepath.Join(fixtureRoot, "bin")
		fixture.stateDir = filepath.Join(fixtureRoot, "state")
		if err := os.Mkdir(fixture.binDir, 0o755); err != nil {
			fixture.err = fmt.Errorf("create ssh conformance bin: %w", err)
			return
		}
		if err := os.Mkdir(fixture.stateDir, 0o755); err != nil {
			fixture.err = fmt.Errorf("create ssh conformance state: %w", err)
			return
		}

		modRoot, err := sshModuleRoot()
		if err != nil {
			fixture.err = err
			return
		}
		sshBin := filepath.Join(fixture.binDir, "ssh")
		cmd := exec.Command("go", "build", "-o", sshBin, "./testdata/fakessh")
		cmd.Dir = filepath.Join(modRoot, "internal", "runtime", "ssh")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fixture.err = fmt.Errorf("building fakessh: %w", err)
		}
	})
	return fixture.err
}

func sshModuleRoot() (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == "/dev/null" {
		return "", fmt.Errorf("not in a Go module")
	}
	return filepath.Dir(filepath.Clean(mod)), nil
}
