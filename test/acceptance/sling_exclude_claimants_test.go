//go:build acceptance_a

package acceptance_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	storesqlite "github.com/gastownhall/gascity/internal/storebinding/sqlite"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// One command-to-store composition proof: the report author is excluded from
// every step of --on, while an eligible peer can finish the entire graph.
func TestSlingOnGraphExcludedAuthorAndEligiblePeer(t *testing.T) {
	env := testEnv.Clone()
	c := helpers.NewCity(t, env)
	c.InitNoStart("claude")
	c.WriteConfig(`[workspace]
name = "review-exclusions"

[daemon]
formula_v2 = true

[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/infra"
`)
	c.WriteV2AgentDir("review", `start_command = "true"`,
		`work_query = "gc ready --json --metadata-field gc.routed_to=review"`)
	formulaDir := filepath.Join(c.Dir, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "review-proof.formula.toml"), []byte(`formula = "review-proof"
version = 2
contract = "graph.v2"

[[steps]]
id = "inspect"
title = "Inspect report"

[[steps]]
id = "verdict"
title = "Write verdict"
needs = ["inspect"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	work, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(c.Dir, ".gc", "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := work.Create(beads.Bead{Title: "report", Type: "task", Metadata: map[string]string{beadmeta.ExcludeClaimantsMetadataKey: "review.author"}})
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, stderr, err := helpers.RunGCStreams(env, c.Dir, args...)
		if err != nil {
			t.Fatalf("gc %v: %v\n%s\n%s", args, err, out, stderr)
		}
		return out
	}
	var launch struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal([]byte(run("sling", "review", source.ID, "--on", "review-proof", "--json")), &launch); err != nil {
		t.Fatal(err)
	}
	if launch.WorkflowID == "" {
		t.Fatal("sling returned no workflow")
	}
	graphPath, err := storesqlite.GraphPath(filepath.Join(c.Dir, ".gc", "infra"))
	if err != nil {
		t.Fatal(err)
	}
	graph, err := beads.OpenSQLiteStore(filepath.Dir(graphPath), beads.WithSQLiteStoreReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closer, ok := graph.(interface{ CloseStore() error }); ok {
			if err := closer.CloseStore(); err != nil {
				t.Error(err)
			}
		}
	})
	members, err := graph.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: launch.WorkflowID}, 0, beads.WithBothTiers)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 {
		t.Fatalf("graph has %d steps, want inspect, verdict, finalizer", len(members))
	}
	var finalizer string
	for _, step := range members {
		if step.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflowFinalize {
			finalizer = step.ID
		}
		env.With("GC_AGENT", "review").With("GC_ALIAS", "review.author").With("BEADS_ACTOR", "review.author")
		out, err := c.GC("bd", "update", step.ID, "--claim")
		if err == nil || !strings.Contains(out, "exclud") {
			t.Fatalf("excluded author's by-id claim of %s: %v\n%s", step.ID, err, out)
		}
	}
	for _, want := range []string{"inspect", "verdict"} {
		env.With("GC_ALIAS", "review.author").With("BEADS_ACTOR", "review.author")
		var claim struct {
			Action string `json:"action"`
			BeadID string `json:"bead_id"`
		}
		if err := json.Unmarshal([]byte(run("hook", "review", "--claim", "--json")), &claim); err != nil {
			t.Fatal(err)
		}
		if claim.Action != "drain" || claim.BeadID != "" {
			t.Fatalf("excluded author claimed %+v", claim)
		}
		env.With("GC_ALIAS", "review.peer").With("BEADS_ACTOR", "review.peer")
		if err := json.Unmarshal([]byte(run("hook", "review", "--claim", "--json")), &claim); err != nil {
			t.Fatal(err)
		}
		if claim.Action != "work" || claim.BeadID == "" {
			t.Fatalf("eligible peer got %+v", claim)
		}
		step, err := graph.Get(claim.BeadID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(step.Metadata[beadmeta.StepRefMetadataKey], "."+want) {
			t.Fatalf("claimed %s, want %s", step.Metadata[beadmeta.StepRefMetadataKey], want)
		}
		run("bd", "update", step.ID, "--set-metadata", "gc.outcome=pass", "--status", "closed")
	}
	if finalizer == "" {
		t.Fatal("missing synthesized finalizer")
	}
	run("convoy", "control", finalizer)
	root, err := graph.Get(launch.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "closed" || root.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("workflow did not complete: %+v", root)
	}
}
