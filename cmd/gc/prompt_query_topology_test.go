package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// promptQueryPinTemplate prints the four discovery placeholders between line
// sentinels so each rendered value can be recovered byte-exactly from a fully
// rendered prompt, not just from a PromptContext struct.
const promptQueryPinTemplate = `__WORK_QUERY_BEGIN__
{{.WorkQuery}}
__WORK_QUERY_END__
__ASSIGNED_IN_PROGRESS_BEGIN__
{{.AssignedInProgressQuery}}
__ASSIGNED_IN_PROGRESS_END__
__ASSIGNED_READY_BEGIN__
{{.AssignedReadyQuery}}
__ASSIGNED_READY_END__
__ROUTED_POOL_BEGIN__
{{.RoutedPoolQuery}}
__ROUTED_POOL_END__
`

// writePromptQueryPinTemplate writes promptQueryPinTemplate under cityPath and
// returns its path relative to the city root.
func writePromptQueryPinTemplate(t *testing.T, cityPath string) string {
	t.Helper()
	dir := filepath.Join(cityPath, "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating prompts dir: %v", err)
	}
	rel := filepath.Join("prompts", "pin.template.md")
	if err := os.WriteFile(filepath.Join(cityPath, rel), []byte(promptQueryPinTemplate), 0o644); err != nil {
		t.Fatalf("writing pin template: %v", err)
	}
	return rel
}

// extractPinnedPromptQuery returns the value rendered between one template's
// sentinel pair, or "" when the pair is absent.
func extractPinnedPromptQuery(rendered, marker string) string {
	open := "__" + marker + "_BEGIN__"
	endMarker := "__" + marker + "_END__"
	i := strings.Index(rendered, open)
	if i < 0 {
		return ""
	}
	rest := rendered[i+len(open):]
	j := strings.Index(rest, endMarker)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// assertCompactDiscoveryQuery checks one discovery placeholder of an agent
// whose prompt must reach work in every store: the prompt carries the compact
// `gc hook query <tier>` command, never a store-shaped `gc ready` or `bd` read.
// The hook resolves the city's storage topology when the agent runs it and fans
// the tier's query out over every store (TestHookQueryStaysSingleStorePerStoreForCityScopedAgents),
// so the rendered prompt never bakes a topology in.
func assertCompactDiscoveryQuery(t *testing.T, name, query, tier string) {
	t.Helper()
	if want := "gc hook query " + tier; query != want {
		t.Errorf("%s = %q, want the compact hook command %q", name, query, want)
	}
}

// promptQueryTestParams builds the minimal agentBuildParams resolveTemplate
// needs for the render sites under test.
func promptQueryTestParams(cityPath string, cfg *config.City) *agentBuildParams {
	return &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        cfg.Workspace.Name,
		cityPath:        cityPath,
		city:            cfg,
		workspace:       &cfg.Workspace,
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/claude", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
		rigs:            cfg.Rigs,
	}
}

// TestPromptQueriesForCityScopedAgentReachRigStores pins AC-1 of issue #1689:
// a city-scoped agent's four discovery placeholders run verbatim in the agent's
// own shell, so on a city with rigs they must reach the city store and every
// rig store instead of the single-store `bd` commands its work_dir resolves.
// The placeholders are the compact `gc hook query <tier>` commands, and the
// hook fans the tier's single-store query out per store when the agent runs
// it, so the rendered prompt carries no store-shaped query at either render
// site.
func TestPromptQueriesForCityScopedAgentReachRigStores(t *testing.T) {
	t.Run("pack-agnostic city agent", func(t *testing.T) {
		cityPath := t.TempDir()
		tmpl := writePromptQueryPinTemplate(t, cityPath)
		rigs := []config.Rig{{Name: "myrig", Path: filepath.Join(t.TempDir(), "myrig")}}
		cfg := &config.City{
			Workspace: config.Workspace{Name: "bright-lights", Provider: "claude"},
			Rigs:      rigs,
		}
		agent := &config.Agent{
			Name:           "planner",
			Scope:          "city",
			Provider:       "claude",
			PromptTemplate: tmpl,
		}
		topo := cityQueryTopology(cityPath, cfg)
		if topo.FederatedReady {
			t.Fatalf("fixture precondition: a temp-dir city relocates no coordination class, so FederatedReady must be false here")
		}

		// Site 1: gc prime's context builder (also drives the prompt-delivery
		// doctor through the same function).
		ctx := buildAgentPromptContext(cityPath, cfg.Workspace.Name, agent, rigs, io.Discard)
		assertCompactDiscoveryQuery(t, "prime WorkQuery", ctx.WorkQuery, "work")
		assertCompactDiscoveryQuery(t, "prime AssignedInProgressQuery", ctx.AssignedInProgressQuery, "assigned-in-progress")
		assertCompactDiscoveryQuery(t, "prime AssignedReadyQuery", ctx.AssignedReadyQuery, "assigned-ready")
		assertCompactDiscoveryQuery(t, "prime RoutedPoolQuery", ctx.RoutedPoolQuery, "routed-pool")

		// Site 2: the controller's session-start render. The placeholders come
		// back inside the rendered prompt, so recover them between the
		// sentinels and require byte-identical values from both sites.
		tp, err := resolveTemplate(promptQueryTestParams(cityPath, cfg), agent, agent.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("resolveTemplate: %v", err)
		}
		sitePairs := []struct {
			marker string
			tier   string
			want   string
		}{
			{"WORK_QUERY", "work", ctx.WorkQuery},
			{"ASSIGNED_IN_PROGRESS", "assigned-in-progress", ctx.AssignedInProgressQuery},
			{"ASSIGNED_READY", "assigned-ready", ctx.AssignedReadyQuery},
			{"ROUTED_POOL", "routed-pool", ctx.RoutedPoolQuery},
		}
		for _, pair := range sitePairs {
			got := extractPinnedPromptQuery(tp.Prompt, pair.marker)
			if got == "" {
				t.Fatalf("controller render lost the %s placeholder; prompt:\n%s", pair.marker, tp.Prompt)
			}
			assertCompactDiscoveryQuery(t, "controller "+pair.marker, got, pair.tier)
			if got != pair.want {
				t.Fatalf("controller %s differs from the gc prime render:\ncontroller: %q\nprime:      %q", pair.marker, got, pair.want)
			}
		}
	})

	t.Run("shipped gastown mayor", func(t *testing.T) {
		clearGCEnv(t)
		t.Setenv("GC_BEADS", "file")
		t.Setenv("GC_DOLT", "skip")
		configureIsolatedRuntimeEnv(t)

		src := gastownExamplePath(t)
		cityPath := filepath.Join(t.TempDir(), "city")
		var initOut, initErr bytes.Buffer
		if code := doInitFromDirWithOptions(src, cityPath, "", &initOut, &initErr, true); code != 0 {
			t.Fatalf("doInitFromDirWithOptions = %d; stderr: %s", code, initErr.String())
		}
		rigPath := filepath.Join(t.TempDir(), "myrig")
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatalf("creating rig dir: %v", err)
		}
		var rigOut, rigErr bytes.Buffer
		if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &rigOut, &rigErr); code != 0 {
			t.Fatalf("doRigAdd = %d; stderr: %s", code, rigErr.String())
		}

		cfg, err := loadCityConfig(cityPath)
		if err != nil {
			t.Fatalf("loadCityConfig: %v", err)
		}
		if len(cfg.Rigs) != 1 {
			t.Fatalf("fixture precondition: len(Rigs) = %d, want 1", len(cfg.Rigs))
		}
		var mayor *config.Agent
		for i := range cfg.Agents {
			if cfg.Agents[i].Name == "mayor" {
				mayor = &cfg.Agents[i]
				break
			}
		}
		if mayor == nil {
			t.Fatal("fixture precondition: the shipped gastown pack declares a mayor agent")
		}
		if !agentIsCrossStoreEligible(mayor) {
			t.Fatalf("fixture precondition: the shipped gastown mayor is city-scoped, got scope %q", mayor.Scope)
		}
		topo := cityQueryTopology(cityPath, cfg)
		if topo.FederatedReady {
			t.Fatal("fixture precondition: this city relocates no coordination class")
		}

		ctx := buildAgentPromptContext(cityPath, cfg.Workspace.Name, mayor, cfg.Rigs, io.Discard)
		assertCompactDiscoveryQuery(t, "gastown mayor WorkQuery", ctx.WorkQuery, "work")
		assertCompactDiscoveryQuery(t, "gastown mayor AssignedInProgressQuery", ctx.AssignedInProgressQuery, "assigned-in-progress")

		tp, err := resolveTemplate(promptQueryTestParams(cityPath, cfg), mayor, mayor.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("resolveTemplate(mayor): %v", err)
		}
		if !strings.Contains(tp.Prompt, ctx.WorkQuery) {
			t.Errorf("the controller-rendered mayor prompt does not carry the WorkQuery gc prime renders; prime renders %q", ctx.WorkQuery)
		}
		if !strings.Contains(tp.Prompt, ctx.AssignedInProgressQuery) {
			t.Errorf("the controller-rendered mayor prompt does not carry the AssignedInProgressQuery gc prime renders; prime renders %q", ctx.AssignedInProgressQuery)
		}
	})
}

// TestCityScopedPromptDiscoveryFailsLoudOnABrokenRigStore pins the trade the
// compact discovery command makes. A city-scoped agent's discovery
// placeholders run `gc hook query <tier>`, which reads every serving rig store
// (one single-store leg per store), so while one rig store cannot be opened
// the command exits non-zero naming that rig — even though the claim the agent
// holds sits in the healthy city store — rather than answer from the stores
// that still read. The healthy-city control runs the same rendered command
// over the same claim and serves it, so the failure belongs to the broken rig,
// not to a command that reads nothing.
func TestCityScopedPromptDiscoveryFailsLoudOnABrokenRigStore(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required to run the generated discovery query")
	}
	gcBin := reexecGCTestBinaryForTests(t)
	// The per-store legs shell `bd`; this stand-in serves the one in_progress
	// claim the test stores, under the agent's identity, and nothing else.
	fakeBD := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(fakeBD, []byte(`#!/bin/sh
case " $* " in
  *" in_progress "*) if [ -n "$FAKE_CLAIM_ID" ]; then printf '[{"id":"%s","title":"claimed city work","type":"task","status":"in_progress","assignee":"planner"}]' "$FAKE_CLAIM_ID"; else printf '[]'; fi ;;
  *) printf '[]' ;;
esac
`), 0o755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	const agentName = "planner"

	// runInProgressQuery adds the city-scoped agent to the city, stores an
	// in_progress claim for it in the city store when withClaim is set, renders
	// the agent's AssignedInProgressQuery the way gc prime does, and runs it as
	// the agent's shell would.
	runInProgressQuery := func(t *testing.T, cityDir string, withClaim bool) (stdout, stderr string, exitCode int, claimID string) {
		t.Helper()
		tomlPath := filepath.Join(cityDir, "city.toml")
		cityToml, err := os.ReadFile(tomlPath)
		if err != nil {
			t.Fatalf("read city.toml: %v", err)
		}
		cityToml = append(cityToml, []byte("\n[[agent]]\nname = \""+agentName+"\"\nscope = \"city\"\nstart_command = \"true\"\n")...)
		if err := os.WriteFile(tomlPath, cityToml, 0o644); err != nil {
			t.Fatalf("write city.toml: %v", err)
		}
		cityStore, err := openStoreAtForCity(cityDir, cityDir)
		if err != nil {
			t.Fatalf("open city store: %v", err)
		}
		if withClaim {
			claim := mustCreateReadyBead(t, cityStore, beads.Bead{Title: "claimed city work", Type: "task"})
			if err := cityStore.Update(claim.ID, beads.UpdateOpts{Status: stringPtr("in_progress"), Assignee: stringPtr(agentName)}); err != nil {
				t.Fatalf("claim %s: %v", claim.ID, err)
			}
			claimID = claim.ID
		}
		cfg, err := loadCityConfig(cityDir, io.Discard)
		if err != nil {
			t.Fatalf("loadCityConfig: %v", err)
		}
		topo := cityQueryTopology(cityDir, cfg)
		if topo.FederatedReady {
			t.Fatal("fixture precondition: this city relocates no coordination class")
		}
		agent := &config.Agent{Name: agentName, Scope: "city"}
		query := buildAgentPromptContext(cityDir, cfg.Workspace.Name, agent, cfg.Rigs, io.Discard).AssignedInProgressQuery
		assertCompactDiscoveryQuery(t, "the city-scoped AssignedInProgressQuery", query, "assigned-in-progress")

		var out, errOut bytes.Buffer
		cmd := exec.Command("sh", "-c", query)
		cmd.Dir = cityDir
		cmd.Env = append(os.Environ(),
			"PATH="+filepath.Dir(gcBin)+string(os.PathListSeparator)+filepath.Dir(fakeBD)+string(os.PathListSeparator)+os.Getenv("PATH"),
			// The command resolves the agent from the session's environment,
			// as it does in the agent's own shell.
			"GC_AGENT="+agentName, "GC_ALIAS="+agentName, "GC_SESSION_ID=", "GC_SESSION_NAME=",
			"FAKE_CLAIM_ID="+claimID,
			// The local read: no controller runs here, and asking for one only
			// waits out its timeout before reading the stores anyway.
			"GC_NO_API=1",
		)
		cmd.Stdout, cmd.Stderr = &out, &errOut
		exitCode = 0
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("running the discovery query: %v", err)
			}
			exitCode = exitErr.ExitCode()
		}
		return out.String(), errOut.String(), exitCode, claimID
	}

	t.Run("healthy rig stores: the claim is served", func(t *testing.T) {
		clearGCEnv(t)
		cityDir := newReadyCityWithRigs(t)
		stdout, stderr, exitCode, claimID := runInProgressQuery(t, cityDir, true)
		if exitCode != 0 {
			t.Fatalf("query exited %d over healthy rig stores; stderr=%s", exitCode, stderr)
		}
		if !strings.Contains(stdout, claimID) {
			t.Fatalf("query served %q, want the agent's city-store claim %s", stdout, claimID)
		}
	})

	// With nothing in the city store, the read goes on to the rig stores, and
	// one it cannot open is a failure, never the "no work" an idle queue gives.
	t.Run("one rig store broken: the query fails naming it", func(t *testing.T) {
		clearGCEnv(t)
		cityDir := newReadyCityWithBrokenRig(t)
		stdout, stderr, exitCode, _ := runInProgressQuery(t, cityDir, false)
		if exitCode == 0 {
			t.Fatalf("query exited 0 with the %q rig store unopenable and printed %q; an answer short a store is the false \"no work\" the per-store read exists to rule out", readyBrokenRigName, stdout)
		}
		// The hook's stdout stays its normalized ready-only shape ("[]" here);
		// the non-zero exit and the named store are what a caller acts on.
		if !strings.Contains(stderr, readyBrokenRigName) {
			t.Errorf("query stderr = %q, want it to name the %q rig store", stderr, readyBrokenRigName)
		}
	})
}
