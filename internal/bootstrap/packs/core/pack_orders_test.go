package core

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/orders"
)

// readOrder parses an order TOML from the embedded pack FS and restores the
// Name the scanner would normally derive from the filename (Parse leaves it
// blank because Name is not a TOML field).
func readOrder(t *testing.T, file string) orders.Order {
	t.Helper()
	data, err := fs.ReadFile(PackFS, "orders/"+file)
	if err != nil {
		t.Fatalf("reading orders/%s: %v", file, err)
	}
	o, err := orders.Parse(data)
	if err != nil {
		t.Fatalf("parsing orders/%s: %v", file, err)
	}
	o.Name = strings.TrimSuffix(file, ".toml")
	return o
}

// TestCoreOrdersValidate asserts every embedded order TOML parses and
// passes structural validation, so a malformed order can never ship in the gc
// binary's bundled core pack.
func TestCoreOrdersValidate(t *testing.T) {
	entries, err := fs.ReadDir(PackFS, "orders")
	if err != nil {
		t.Fatalf("reading orders dir: %v", err)
	}
	saw := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		saw = true
		o := readOrder(t, e.Name())
		if err := orders.Validate(o); err != nil {
			t.Errorf("order %s failed validation: %v", e.Name(), err)
		}
	}
	if !saw {
		t.Fatal("no order TOML files found in embedded pack")
	}
}

// assertEventExecOrder checks an event-triggered exec order: it must validate,
// listen for the expected event type, dispatch via exec (not a formula/pool),
// and point at a script that is actually embedded in the pack.
func assertEventExecOrder(t *testing.T, orderFile, eventType, scriptBase string) {
	t.Helper()
	o := readOrder(t, orderFile)
	if err := orders.Validate(o); err != nil {
		t.Fatalf("%s failed validation: %v", orderFile, err)
	}
	if o.Trigger != "event" {
		t.Errorf("%s: trigger = %q, want %q", orderFile, o.Trigger, "event")
	}
	if o.On != eventType {
		t.Errorf("%s: on = %q, want %q", orderFile, o.On, eventType)
	}
	if !o.IsExec() {
		t.Errorf("%s: want exec dispatch, got formula %q", orderFile, o.Formula)
	}
	if o.Pool != "" {
		t.Errorf("%s: exec orders must not set a pool, got %q", orderFile, o.Pool)
	}
	wantSuffix := "assets/scripts/" + scriptBase
	if !strings.HasSuffix(o.Exec, wantSuffix) {
		t.Errorf("%s: exec = %q, want suffix %q", orderFile, o.Exec, wantSuffix)
	}
	if _, err := fs.ReadFile(PackFS, "assets/scripts/"+scriptBase); err != nil {
		t.Errorf("%s: referenced script not embedded: %v", orderFile, err)
	}
}

// TestNudgeOnRouteOrder pins the nudge-on-route order's event contract: it wakes
// on bead.updated and runs the nudge-on-route script.
func TestNudgeOnRouteOrder(t *testing.T) {
	assertEventExecOrder(t, "nudge-on-route.toml", "bead.updated", "nudge-on-route.sh")
}

// TestCascadeNudgeOnBlockerCloseOrder pins the cascade-nudge order's event
// contract: it wakes on bead.closed — the event the close transition actually
// emits — and runs the cascade-nudge script.
func TestCascadeNudgeOnBlockerCloseOrder(t *testing.T) {
	assertEventExecOrder(t, "cascade-nudge-on-blocker-close.toml", "bead.closed", "cascade-nudge-on-blocker-close.sh")
}

// TestCascadeNudgeRoutesCrossRig guards the cascade order's cross-rig
// routing. Two properties must hold or cross-rig cascades break silently
// (failures are soft-skipped via `|| continue`, so a regression is invisible
// at runtime): (1) the dependent lookup runs through the `gc bd` wrapper, not
// bare `bd` — `--rig` is a gc flag, not a bd flag, and the wrapper runs bd in
// the owning rig's directory; (2) the prefix->rig lookup excludes the HQ entry
// (`gc rig list` reports the city root as an hq=true pseudo-rig that
// `gc --rig <cityName>` cannot resolve), matching orphan-sweep.sh's
// `select(.hq == false)` convention.
func TestCascadeNudgeRoutesCrossRig(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/cascade-nudge-on-blocker-close.sh")
	if err != nil {
		t.Fatalf("reading cascade-nudge-on-blocker-close.sh: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "gc bd dep list") {
		t.Error("cascade-nudge script must route the dep lookup through `gc bd dep list`; missing")
	}
	if strings.Contains(body, "$(bd dep list") {
		t.Error("cascade-nudge script must not run bare `bd dep list` (--rig is a gc flag, not a bd flag)")
	}
	if !strings.Contains(body, ".hq != true") {
		t.Error("cascade-nudge script must exclude the HQ entry from the prefix->rig lookup; missing `.hq != true`")
	}
}

// TestNudgeOnRouteResolvesPoolMembers guards the pool-base fan-out: a
// multi-session pool routes to the pool BASE (sling's NormalizePoolRouteTarget
// collapses slot -> base), which is the members' template, not a session name
// `gc session nudge` can resolve. The script must therefore enumerate pool
// members by template before nudging — a naive `gc session nudge "$routed_to"`
// silently no-ops for exactly the warm-idle pool workers this order targets.
func TestNudgeOnRouteResolvesPoolMembers(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/nudge-on-route.sh")
	if err != nil {
		t.Fatalf("reading nudge-on-route.sh: %v", err)
	}
	body := string(data)
	for _, want := range []string{"gc session list", "--template"} {
		if !strings.Contains(body, want) {
			t.Errorf("nudge-on-route.sh must resolve pool members; missing %q", want)
		}
	}
}

// TestStudioPipelineAdvanceOrder pins the studio-pipeline-advance order: a
// cooldown exec order (no LLM/agent/pool) that runs the studio-pipeline-advance
// script. The order is the studio pipeline engine — route a gated phase bead
// once its gate-blocker closes — so shipping it with the wrong trigger or a
// missing script would silently stall every phase handoff.
func TestStudioPipelineAdvanceOrder(t *testing.T) {
	o := readOrder(t, "studio-pipeline-advance.toml")
	if err := orders.Validate(o); err != nil {
		t.Fatalf("studio-pipeline-advance.toml failed validation: %v", err)
	}
	if o.Trigger != "cooldown" {
		t.Errorf("trigger = %q, want %q", o.Trigger, "cooldown")
	}
	if o.Interval != "10m" {
		t.Errorf("interval = %q, want %q", o.Interval, "10m")
	}
	if !o.IsExec() {
		t.Errorf("want exec dispatch, got formula %q", o.Formula)
	}
	if o.Pool != "" {
		t.Errorf("exec orders must not set a pool, got %q", o.Pool)
	}
	const scriptBase = "studio-pipeline-advance.sh"
	if !strings.HasSuffix(o.Exec, "assets/scripts/"+scriptBase) {
		t.Errorf("exec = %q, want suffix assets/scripts/%s", o.Exec, scriptBase)
	}
	if _, err := fs.ReadFile(PackFS, "assets/scripts/"+scriptBase); err != nil {
		t.Errorf("referenced script not embedded: %v", err)
	}
}

// TestStudioPipelineAdvanceRoutesCrossLedger guards the pipeline engine's
// load-bearing invariants. The script is mechanical and soft-fails per scope
// (`|| continue`, `|| return 0`, stderr hidden), so a regression is invisible
// at runtime — it just silently stops advancing phases. Pin: (1) `gc bd show`
// output is array-normalized before field extraction — the CLI returns a
// single-element array, so a bare `.status` / `.metadata[...]` errors and every
// gate reads as "not closed"; (2) the rig fan-out excludes the HQ pseudo-rig
// (`select(.hq == false)`), matching the orphan-sweep / cross-rig-deps
// convention; (3) on advance the script both slings the bead and clears
// gc.pipeline_gate, so the order fires exactly once.
func TestStudioPipelineAdvanceRoutesCrossLedger(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/studio-pipeline-advance.sh")
	if err != nil {
		t.Fatalf("reading studio-pipeline-advance.sh: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, `if type == "array" then .[0] else . end`) {
		t.Error("script must array-normalize `gc bd show` output before extraction; missing the `if type == \"array\"` guard")
	}
	if !strings.Contains(body, "select(.hq == false)") {
		t.Error("script must exclude the HQ pseudo-rig from the rig fan-out; missing `select(.hq == false)`")
	}
	if !strings.Contains(body, "gc sling") {
		t.Error("script must sling the gated bead to its route; missing `gc sling`")
	}
	if !strings.Contains(body, "--unset-metadata gc.pipeline_gate") {
		t.Error("script must clear gc.pipeline_gate after routing for fire-once idempotence; missing `--unset-metadata gc.pipeline_gate`")
	}
}
