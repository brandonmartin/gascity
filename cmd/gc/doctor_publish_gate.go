package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/publishgate"
)

// publishGateFixHint is deliberately self-contained: the operator reading it
// is usually the mayor deciding, right now, whether to publish or re-dispatch.
const publishGateFixHint = "A gate hold older than the artifact's tolerable drift is a defect, not a queue. " +
	"Publish it (push the SHA the check reports, NOT metadata.branch, which can point at pre-rebase history), " +
	"or re-dispatch the bead for a fresh rebase. Artifacts missing branch_ready_at/target_head were halted " +
	"without provenance — re-stamp them so the next reader does not have to reconstruct it with rev-list."

// publishGateCheck reports artifacts parked at a publish gate: how long each
// has waited, how far its target has moved since, and which SHA is actually
// publishable.
//
// Gas Town holds irreversible publishes behind a single accountable actor, so
// a finished branch waits on a human-paced gate while its target advances
// (~38 commits/day on the gascity rig). Three branch lineages died of that
// drift before anything measured it — the gate had no clock and no staleness
// number, so every agent that touched a held bead re-derived "how far behind
// is this?" by hand, or shipped without asking. See ga-qbq.
//
// It classifies bead content through the typed beads.Store interface, so it
// lives in cmd/gc alongside holdLabelConventionsCheck rather than in
// internal/doctor.
type publishGateCheck struct {
	// dir is the rig's bead store root; repoDir is the git repository the
	// artifact's refs live in. They are the same path for a normal rig and
	// separable only for tests.
	dir      string
	repoDir  string
	label    string
	newStore func(string) (beads.Store, error)
	// newResolver is injectable so tests can assess without a real repo.
	newResolver func(repoDir string) publishgate.Resolver
	thresholds  publishgate.Thresholds
	now         func() time.Time
}

func newPublishGateCheck(dir, label string, newStore func(string) (beads.Store, error)) *publishGateCheck {
	return &publishGateCheck{
		dir:      dir,
		repoDir:  dir,
		label:    label,
		newStore: newStore,
		newResolver: func(repoDir string) publishgate.Resolver {
			return publishgate.NewGitResolver(repoDir)
		},
		thresholds: publishgate.DefaultThresholds(),
		now:        time.Now,
	}
}

func (c *publishGateCheck) Name() string { return "publish-gate:" + c.label }

func (c *publishGateCheck) CanFix() bool { return false }

// Fix is a no-op. The remediations are publish, re-dispatch, and re-stamp —
// all of them judgment calls about someone else's unmerged work.
func (c *publishGateCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible returns false: a days-old gate hold is not a reason to slow
// down `gc start`, and it will surface on the next doctor run either way.
func (c *publishGateCheck) WarmupEligible() bool { return false }

func (c *publishGateCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	res := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}

	if c.newStore == nil || strings.TrimSpace(c.dir) == "" {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("publish-gate holds unknown for %s: no bead store configured", c.label)
		return res
	}
	store, err := c.newStore(c.dir)
	if err != nil {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("publish-gate holds unknown for %s: opening bead store: %v", c.label, err)
		return res
	}

	// One full non-closed scan rather than per-key metadata queries: a gate
	// hold is marked by branch_ready=true OR by a free-form halt_reason
	// ending in "_gate" (mayor_publish_gate, operator_gate, ...), and an
	// exact-match metadata query cannot express the suffix.
	all, err := store.ListOpen()
	if err != nil {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("publish-gate holds unknown for %s: listing beads: %v", c.label, err)
		return res
	}

	var resolver publishgate.Resolver
	if c.newResolver != nil && strings.TrimSpace(c.repoDir) != "" {
		resolver = c.newResolver(c.repoDir)
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}

	assessments := make([]publishgate.Assessment, 0, 4)
	for _, b := range all {
		artifact, held := publishgate.ArtifactFromBead(b)
		if !held {
			continue
		}
		assessments = append(assessments, publishgate.Assess(artifact, resolver, now(), c.thresholds))
	}
	if len(assessments) == 0 {
		res.Status = doctor.StatusOK
		res.Message = fmt.Sprintf("no artifacts held at a publish gate in %s", c.label)
		return res
	}
	sortPublishGateAssessments(assessments)

	worst := assessments[0]
	details := make([]string, 0, len(assessments)*2)
	for _, a := range assessments {
		details = append(details, a.Line())
		for _, note := range a.Notes {
			details = append(details, "    "+note)
		}
	}
	res.Details = details

	switch worst.Severity {
	case publishgate.SeverityEscalate:
		res.Status = doctor.StatusError
		res.FixHint = publishGateFixHint
	case publishgate.SeverityWarn:
		res.Status = doctor.StatusWarning
		res.FixHint = publishGateFixHint
	default:
		res.Status = doctor.StatusOK
	}
	res.Message = fmt.Sprintf("%s held at a publish gate in %s; worst: %s",
		pluralArtifacts(len(assessments)), c.label, worst.Line())
	return res
}

// sortPublishGateAssessments puts the artifact most in need of a decision
// first: highest severity, then longest wait, then bead ID for stability.
func sortPublishGateAssessments(a []publishgate.Assessment) {
	sort.SliceStable(a, func(i, j int) bool {
		if a[i].Severity != a[j].Severity {
			return a[i].Severity > a[j].Severity
		}
		if a[i].AgeKnown != a[j].AgeKnown {
			// An unknown age is a missing clock, not a young artifact.
			return !a[i].AgeKnown
		}
		if a[i].AgeKnown && a[i].Age != a[j].Age {
			return a[i].Age > a[j].Age
		}
		return a[i].BeadID < a[j].BeadID
	})
}

func pluralArtifacts(n int) string {
	if n == 1 {
		return "1 artifact"
	}
	return fmt.Sprintf("%d artifacts", n)
}
