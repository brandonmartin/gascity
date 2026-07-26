package publishgate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// fakeResolver answers from fixed maps so assessment logic can be tested
// without a repository.
type fakeResolver struct {
	refs   map[string]string
	counts map[string]int
}

func (f fakeResolver) ResolveRef(ref string) (string, error) {
	if sha, ok := f.refs[ref]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("%w: %s", ErrUnresolvedRef, ref)
}

func (f fakeResolver) CountBehind(from, to string) (int, error) {
	if n, ok := f.counts[from+".."+to]; ok {
		return n, nil
	}
	return 0, fmt.Errorf("no count for %s..%s", from, to)
}

const (
	artifactSHA = "f5289f71058c6cf5236b0c50df0563c6b0bc9191"
	preRebase   = "ec90bac29981c4f6a1da2fa6e6d7228a0be6d31a"
	targetSHA   = "1bc642727251a84a33d0316a0d3d26e3e9c11ffe"
	haltSHA     = "aaaa111122223333444455556666777788889999"
)

func healthyArtifact() Artifact {
	return Artifact{
		BeadID:     "ga-b5h",
		Title:      "rebase upstream PR",
		Branch:     "fix/order-cooldown-event-fallback",
		Commit:     artifactSHA,
		Target:     "upstream/main",
		TargetHead: haltSHA,
		HaltReason: "mayor_publish_gate",
		ReadyAt:    time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC),
	}
}

// healthyResolver publishes the artifact at the branch tip, so nothing is
// stale and every measurement resolves.
func healthyResolver() fakeResolver {
	return fakeResolver{
		refs: map[string]string{
			artifactSHA:                  artifactSHA,
			"refs/remotes/upstream/main": targetSHA,
			"refs/remotes/origin/fix/order-cooldown-event-fallback": artifactSHA,
			"refs/remotes/origin/develop":                           targetSHA,
			"refs/remotes/origin/polecat/ga-qbq":                    artifactSHA,
		},
		counts: map[string]int{
			artifactSHA + ".." + targetSHA: 2,
			haltSHA + ".." + targetSHA:     3,
		},
	}
}

func TestAssessHealthyHoldIsQuiet(t *testing.T) {
	now := time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC)
	got := Assess(healthyArtifact(), healthyResolver(), now, Thresholds{})

	if got.Severity != SeverityOK {
		t.Fatalf("Severity = %v, want ok (notes: %v)", got.Severity, got.Notes)
	}
	if len(got.Notes) != 0 {
		t.Fatalf("Notes = %v, want none", got.Notes)
	}
	if !got.AgeKnown || got.Age != 6*time.Hour {
		t.Errorf("Age = %v (known=%v), want 6h", got.Age, got.AgeKnown)
	}
	if !got.BehindKnown || got.Behind != 2 {
		t.Errorf("Behind = %d (known=%v), want 2", got.Behind, got.BehindKnown)
	}
	if !got.MovedSinceHaltKnown || got.MovedSinceHalt != 3 {
		t.Errorf("MovedSinceHalt = %d (known=%v), want 3", got.MovedSinceHalt, got.MovedSinceHaltKnown)
	}
	if got.PublishSHA != artifactSHA || got.PublishSHASource != "metadata.commit" {
		t.Errorf("PublishSHA = %s from %s, want %s from metadata.commit", got.PublishSHA, got.PublishSHASource, artifactSHA)
	}
	if got.BranchStale {
		t.Errorf("BranchStale = true, want false when the branch tip is the artifact")
	}
}

func TestAssessAgeThresholds(t *testing.T) {
	ready := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		want Severity
	}{
		{"fresh", ready.Add(2 * time.Hour), SeverityOK},
		{"just before warn", ready.Add(DefaultWarnAfter - time.Minute), SeverityOK},
		{"at warn", ready.Add(DefaultWarnAfter), SeverityWarn},
		{"just before escalate", ready.Add(DefaultEscalateAfter - time.Minute), SeverityWarn},
		{"at escalate", ready.Add(DefaultEscalateAfter), SeverityEscalate},
		{"fifteen days", ready.Add(15 * 24 * time.Hour), SeverityEscalate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := healthyArtifact()
			a.ReadyAt = ready
			got := Assess(a, healthyResolver(), tc.now, Thresholds{})
			if got.Severity != tc.want {
				t.Fatalf("Severity = %v, want %v (notes: %v)", got.Severity, tc.want, got.Notes)
			}
		})
	}
}

// A gate with no recorded arrival time is the failure ga-qbq was filed
// about: nothing distinguishes a fresh artifact from one that waited two
// weeks, because the bead's own updated_at moves on every touch.
func TestAssessMissingReadyAtWarnsAndReportsUnknownAge(t *testing.T) {
	a := healthyArtifact()
	a.ReadyAt = time.Time{}
	got := Assess(a, healthyResolver(), time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC), Thresholds{})

	if got.Severity != SeverityWarn {
		t.Fatalf("Severity = %v, want warn", got.Severity)
	}
	if got.AgeKnown {
		t.Errorf("AgeKnown = true, want false")
	}
	if !containsSubstring(got.Notes, MetaBranchReadyAt) {
		t.Errorf("Notes = %v, want one naming %s", got.Notes, MetaBranchReadyAt)
	}
	if !strings.Contains(got.Line(), "unknown time") {
		t.Errorf("Line() = %q, want it to say the wait is unknown", got.Line())
	}
}

// The ga-b5h shape: metadata.branch resolved on origin to pre-rebase
// history while metadata.commit held the real artifact. A publisher
// following the obvious field would have silently defeated the rebase.
func TestAssessFlagsBranchPointingAtPreRebaseHistory(t *testing.T) {
	r := healthyResolver()
	r.refs["refs/remotes/origin/fix/order-cooldown-event-fallback"] = preRebase

	got := Assess(healthyArtifact(), r, time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC), Thresholds{})

	if !got.BranchStale || !got.BranchStateKnown {
		t.Fatalf("BranchStale = %v (known=%v), want stale", got.BranchStale, got.BranchStateKnown)
	}
	if got.Severity != SeverityWarn {
		t.Errorf("Severity = %v, want warn when metadata.branch is not publishable", got.Severity)
	}
	if got.PublishSHA != artifactSHA {
		t.Errorf("PublishSHA = %s, want the artifact %s", got.PublishSHA, artifactSHA)
	}
	if !containsSubstring(got.Notes, shortSHA(preRebase)) {
		t.Errorf("Notes = %v, want one naming the pre-rebase SHA", got.Notes)
	}
	if !strings.Contains(got.Line(), "metadata.branch is stale") {
		t.Errorf("Line() = %q, want a stale-branch marker", got.Line())
	}
}

func TestAssessFlagsUnpublishedBranch(t *testing.T) {
	r := healthyResolver()
	delete(r.refs, "refs/remotes/origin/fix/order-cooldown-event-fallback")

	got := Assess(healthyArtifact(), r, time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC), Thresholds{})

	if !got.BranchStale {
		t.Fatalf("BranchStale = false, want true for an unpushed branch")
	}
	if !containsSubstring(got.Notes, "not published on any remote") {
		t.Errorf("Notes = %v, want one saying the branch is unpublished", got.Notes)
	}
}

func TestAssessMissingTargetHeadWarns(t *testing.T) {
	a := healthyArtifact()
	a.TargetHead = ""
	got := Assess(a, healthyResolver(), time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC), Thresholds{})

	if got.Severity != SeverityWarn {
		t.Fatalf("Severity = %v, want warn", got.Severity)
	}
	if got.MovedSinceHaltKnown {
		t.Errorf("MovedSinceHaltKnown = true, want false without a recorded target head")
	}
	if !containsSubstring(got.Notes, MetaTargetHead) {
		t.Errorf("Notes = %v, want one naming %s", got.Notes, MetaTargetHead)
	}
	// The behind-count still resolves: target_head is a convenience, not the
	// only route to a staleness number.
	if !got.BehindKnown || got.Behind != 2 {
		t.Errorf("Behind = %d (known=%v), want 2", got.Behind, got.BehindKnown)
	}
}

// A bare target names origin's copy; a remote-qualified one names that
// remote. ga-b5h recorded "upstream/main", which must not be read as
// "refs/remotes/origin/upstream/main".
func TestAssessResolvesBareAndRemoteQualifiedTargets(t *testing.T) {
	a := healthyArtifact()
	a.Target = "develop"
	a.Branch = "polecat/ga-qbq"
	a.TargetHead = haltSHA
	got := Assess(a, healthyResolver(), time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC), Thresholds{})
	if got.TargetTip != targetSHA {
		t.Fatalf("TargetTip = %q, want %s for a bare target", got.TargetTip, targetSHA)
	}

	got = Assess(healthyArtifact(), healthyResolver(), time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC), Thresholds{})
	if got.TargetTip != targetSHA {
		t.Fatalf("TargetTip = %q, want %s for a remote-qualified target", got.TargetTip, targetSHA)
	}
}

func TestAssessMissingCommitFallsBackToBranch(t *testing.T) {
	a := healthyArtifact()
	a.Commit = ""
	got := Assess(a, healthyResolver(), time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC), Thresholds{})

	if got.PublishSHA != artifactSHA {
		t.Fatalf("PublishSHA = %q, want the branch tip %s", got.PublishSHA, artifactSHA)
	}
	if got.PublishSHASource == "metadata."+MetaCommit {
		t.Errorf("PublishSHASource = %q, want the branch ref", got.PublishSHASource)
	}
	// Branch authority is meaningless when the SHA came from the branch.
	if got.BranchStateKnown {
		t.Errorf("BranchStateKnown = true, want false when there is nothing independent to compare")
	}
	if got.Severity != SeverityWarn {
		t.Errorf("Severity = %v, want warn for missing provenance", got.Severity)
	}
}

func TestAssessTargetThatDoesNotResolveWarns(t *testing.T) {
	a := healthyArtifact()
	a.Target = "release/never-fetched"
	got := Assess(a, healthyResolver(), time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC), Thresholds{})

	if got.Severity != SeverityWarn {
		t.Fatalf("Severity = %v, want warn", got.Severity)
	}
	if got.BehindKnown {
		t.Errorf("BehindKnown = true, want false when the target does not resolve")
	}
	if !strings.Contains(got.Line(), "drift vs release/never-fetched unknown") {
		t.Errorf("Line() = %q, want it to name the unmeasured target", got.Line())
	}
}

func TestAssessNilResolverStillApplies(t *testing.T) {
	a := healthyArtifact()
	got := Assess(a, nil, a.ReadyAt.Add(100*time.Hour), Thresholds{})

	if got.Severity != SeverityEscalate {
		t.Fatalf("Severity = %v, want escalate from age alone", got.Severity)
	}
	if got.PublishSHA != artifactSHA {
		t.Errorf("PublishSHA = %q, want metadata.commit passed through", got.PublishSHA)
	}
	if got.BehindKnown || got.BranchStateKnown {
		t.Errorf("git-derived fields should stay unknown without a resolver")
	}
}

func TestThresholdsNormalize(t *testing.T) {
	got := Thresholds{}.normalized()
	if got.WarnAfter != DefaultWarnAfter || got.EscalateAfter != DefaultEscalateAfter {
		t.Fatalf("zero Thresholds = %+v, want defaults", got)
	}
	// An escalate threshold below the warn threshold would make warnings
	// unreachable; clamp instead of inverting the ladder.
	got = Thresholds{WarnAfter: 10 * time.Hour, EscalateAfter: time.Hour}.normalized()
	if got.EscalateAfter != 10*time.Hour {
		t.Fatalf("EscalateAfter = %v, want it clamped up to WarnAfter", got.EscalateAfter)
	}
}

func TestArtifactFromBead(t *testing.T) {
	cases := []struct {
		name string
		meta beads.StringMap
		want bool
	}{
		{"branch_ready true", beads.StringMap{MetaBranchReady: "true"}, true},
		{"branch_ready mixed case", beads.StringMap{MetaBranchReady: "True"}, true},
		{"branch_ready false", beads.StringMap{MetaBranchReady: "false"}, false},
		{"gate halt reason only", beads.StringMap{MetaHaltReason: "mayor_publish_gate"}, true},
		{"non-gate halt reason", beads.StringMap{MetaHaltReason: "auto_push_false"}, false},
		{"unrelated bead", beads.StringMap{"branch": "polecat/ga-qbq"}, false},
		{"no metadata", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, held := ArtifactFromBead(beads.Bead{ID: "ga-1", Metadata: tc.meta})
			if held != tc.want {
				t.Fatalf("held = %v, want %v", held, tc.want)
			}
		})
	}
}

func TestArtifactFromBeadParsesFields(t *testing.T) {
	got, held := ArtifactFromBead(beads.Bead{
		ID:    " ga-b5h ",
		Title: " rebase ",
		Metadata: beads.StringMap{
			MetaBranchReady:   "true",
			MetaBranchReadyAt: "2026-07-11T09:30:00Z",
			MetaBranch:        "fix/order-cooldown-event-fallback",
			MetaCommit:        artifactSHA,
			MetaTarget:        "upstream/main",
			MetaTargetHead:    haltSHA,
			MetaHaltReason:    "mayor_publish_gate",
		},
	})
	if !held {
		t.Fatal("held = false, want true")
	}
	if got.BeadID != "ga-b5h" || got.Title != "rebase" {
		t.Errorf("BeadID/Title = %q/%q, want trimmed values", got.BeadID, got.Title)
	}
	want := time.Date(2026, 7, 11, 9, 30, 0, 0, time.UTC)
	if !got.ReadyAt.Equal(want) {
		t.Errorf("ReadyAt = %v, want %v", got.ReadyAt, want)
	}
}

func TestArtifactFromBeadIgnoresUnparsableReadyAt(t *testing.T) {
	got, held := ArtifactFromBead(beads.Bead{
		ID: "ga-1",
		Metadata: beads.StringMap{
			MetaBranchReady:   "true",
			MetaBranchReadyAt: "last tuesday",
		},
	})
	if !held {
		t.Fatal("held = false, want true")
	}
	if !got.ReadyAt.IsZero() {
		t.Fatalf("ReadyAt = %v, want zero for an unparsable stamp", got.ReadyAt)
	}
}

func TestFormatAge(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0m"},
		{-time.Hour, "0m"},
		{45 * time.Minute, "45m"},
		{90 * time.Minute, "1h30m"},
		{25 * time.Hour, "1d1h"},
		{15 * 24 * time.Hour, "15d0h"},
	}
	for _, tc := range cases {
		if got := formatAge(tc.in); got != tc.want {
			t.Errorf("formatAge(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSeverityString(t *testing.T) {
	for sev, want := range map[Severity]string{SeverityOK: "ok", SeverityWarn: "warn", SeverityEscalate: "escalate"} {
		if got := sev.String(); got != want {
			t.Errorf("Severity(%d).String() = %q, want %q", sev, got, want)
		}
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
