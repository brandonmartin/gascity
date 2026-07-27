package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCensusOwnerLiveness runs the shell self-test for
// scripts/check-census-owner-liveness.sh, the census-owner-liveness order
// wrapper. It pins the dedup contract that regressed in ga-xw25: exactly one
// alert bead per distinct dangling owner_bead, never re-filed while any bead
// already names that owner_bead — in any status, with or without the patrol's
// own label. Hermetic: fake `gc` and `bd` on PATH and temp dirs only, no
// network and no real bead store.
func TestCensusOwnerLiveness(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command(filepath.Join(root, "scripts", "test-census-owner-liveness.sh"))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-census-owner-liveness.sh failed: %v\n%s", err, out)
	}
}
