package scripts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// censusLivenessScript is the order wrapper under test, named relative to
// scripts/ so it can be handed straight to scriptCommand.
const censusLivenessScript = "check-census-owner-liveness.sh"

// censusDoctorJSON is a minimal `gc doctor --json` payload carrying one
// dangling owner_bead finding, in the exact shape
// cmd/gc/doctor_census_owner_liveness.go emits:
// "%s: dangling owner_bead=%s rows=[%s]".
const censusDoctorJSON = `{
  "results": [
    {
      "name": "census-owner-liveness",
      "status": "warning",
      "details": ["city: dangling owner_bead=ga-missing-1 rows=[row-a; row-b]"]
    }
  ]
}`

// runCensusLiveness executes the patrol with fake `gc` and `bd` binaries on
// PATH and returns the metadata JSON the fake `bd create` was handed. Nothing
// touches the real bead store or a real city.
func runCensusLiveness(t *testing.T, extraEnv ...string) string {
	t.Helper()

	binDir := t.TempDir()
	record := filepath.Join(t.TempDir(), "metadata.json")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env sh
# Only `+"`gc doctor --json`"+` is used by the patrol.
cat <<'DOCTOR_JSON'
`+censusDoctorJSON+`
DOCTOR_JSON
`)

	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env sh
case "$1" in
  list)
    # No pre-existing open alert for this owner_bead.
    printf '[]\n'
    ;;
  create)
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "--metadata" ]; then
        printf '%s' "$2" > "$CENSUS_METADATA_RECORD"
        break
      fi
      shift
    done
    printf 'ga-fake-alert\n'
    ;;
  *)
    printf 'unexpected bd subcommand: %s\n' "$1" >&2
    exit 1
    ;;
esac
`)

	// scriptCommand rather than a fresh exec.Command: the P0.4 resource ledger
	// ratchets untagged subprocess call/file totals and they cannot grow.
	root := repoRoot(t)
	cmd := scriptCommand(root, censusLivenessScript)
	cmd.Dir = root
	cmd.Env = append([]string{
		"PATH=" + binDir + ":/usr/bin:/bin",
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"CENSUS_METADATA_RECORD=" + record,
	}, extraEnv...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v\n%s", censusLivenessScript, err, out)
	}

	metadata, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("patrol filed no alert (no --metadata recorded): %v\nscript output:\n%s", err, out)
	}
	return string(metadata)
}

// decodeCensusMetadata parses the recorded --metadata payload, failing the test
// if the patrol handed `bd create` anything but a JSON object.
func decodeCensusMetadata(t *testing.T, raw string) map[string]any {
	t.Helper()
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("decode --metadata %q: %v", raw, err)
	}
	return meta
}

// TestCensusOwnerLivenessOmitsRouteByDefault is the regression guard for
// ga-01ms. The patrol used to default gc.routed_to to a hardcoded
// "gascity/architect" — an agent present in no city.toml, no pack, and no
// session — so every alert it filed carried a route that resolved to nothing
// and could never be claimed. With no operator-supplied target the patrol must
// leave gc.routed_to off the bead entirely, which drops the alert into the
// rig's normal unassigned pool alongside every other dispatchable bead.
func TestCensusOwnerLivenessOmitsRouteByDefault(t *testing.T) {
	meta := decodeCensusMetadata(t, runCensusLiveness(t))

	if route, ok := meta["gc.routed_to"]; ok {
		t.Fatalf("gc.routed_to = %v, want the key absent so the alert lands in the normal pool", route)
	}
	if got := meta["census.owner_bead"]; got != "ga-missing-1" {
		t.Fatalf("census.owner_bead = %v, want ga-missing-1", got)
	}
}

// TestCensusOwnerLivenessStampsExplicitRoute pins the operator escape hatch:
// a city that really does run a dedicated triage agent sets
// CENSUS_OWNER_LIVENESS_ROUTED_TO in the order environment, and that value
// still reaches the bead.
func TestCensusOwnerLivenessStampsExplicitRoute(t *testing.T) {
	meta := decodeCensusMetadata(t, runCensusLiveness(t,
		"CENSUS_OWNER_LIVENESS_ROUTED_TO=gascity/gastown.polecat"))

	if got := meta["gc.routed_to"]; got != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to = %v, want gascity/gastown.polecat", got)
	}
	if got := meta["census.owner_bead"]; got != "ga-missing-1" {
		t.Fatalf("census.owner_bead = %v, want ga-missing-1", got)
	}
}
