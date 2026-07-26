// Package publishgate measures how long a branch-ready artifact has been
// waiting at a publish gate and how far its merge target has moved since.
//
// Gas Town splits irreversible publishes: a worker produces a verified
// branch and halts, and a single accountable actor (the mayor) performs the
// push. That split is deliberate, but the wait it creates had no clock and
// no staleness measure, so artifacts rotted at the gate while their target
// advanced — three branch lineages died that way before this package existed
// (ga-qbq). Upstream velocity on the gascity rig was measured at ~38
// commits/day, so a gate held for a few days is a rebase that has to be
// redone from scratch.
//
// The package answers four questions about a gate-held bead, without the
// reader having to reconstruct it with rev-list by hand:
//
//  1. How long has it been waiting? (age, from branch_ready_at)
//  2. How far behind its target is the artifact now? (commits behind)
//  3. Which SHA must actually be published? (metadata.commit, not
//     necessarily metadata.branch)
//  4. Does metadata.branch still point at pre-rebase history?
//
// Ref resolution is local-only: it reads remote-tracking refs from the
// repository's object store and never talks to a remote. That keeps callers
// (doctor checks, CLI stamps) fast, hermetic, and offline-safe. The cost is
// that a repository which has not fetched recently under-reports drift;
// every Assessment therefore carries the target tip it measured against so
// a stale measurement is visible rather than silently wrong.
package publishgate

import (
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Bead metadata keys that make up the publish-gate contract. The halting
// agent writes them; readers (doctor, publishers) consume them.
const (
	// MetaBranchReady marks a bead as holding a finished, unpublished
	// artifact. "true" is the only value that counts as held.
	MetaBranchReady = "branch_ready"
	// MetaBranchReadyAt is the RFC3339 instant the artifact reached the
	// gate. Without it a gate has no clock: the bead's own UpdatedAt moves
	// every time anyone touches it, so it reads as fresh no matter how long
	// the artifact has actually waited.
	MetaBranchReadyAt = "branch_ready_at"
	// MetaBranch is the branch the work lives on. It is NOT automatically
	// the publishable ref — see MetaBranchStale.
	MetaBranch = "branch"
	// MetaCommit is the artifact SHA. When present it is authoritative:
	// it is the commit a publisher must ship.
	MetaCommit = "commit"
	// MetaTarget is the branch the artifact is meant to land on, either
	// bare ("develop") or remote-qualified ("upstream/main").
	MetaTarget = "target"
	// MetaTargetHead is the target's SHA at the moment the artifact was
	// rebased and halted. Recording it turns "how much has the target moved
	// since this was verified?" into a lookup instead of a hand reconstruction.
	MetaTargetHead = "target_head"
	// MetaBranchStale records, at halt time, that MetaBranch does not
	// resolve to MetaCommit on the remote — i.e. following the obvious
	// field would publish the wrong history.
	MetaBranchStale = "branch_stale"
	// MetaHaltReason names why the agent stopped short of publishing.
	MetaHaltReason = "halt_reason"
)

// Default gate thresholds. Chosen against ~38 commits/day of observed
// target velocity: a day of drift is a nuisance rebase, three days is a
// different branch.
const (
	// DefaultWarnAfter is the age at which a held artifact is worth a warning.
	DefaultWarnAfter = 24 * time.Hour
	// DefaultEscalateAfter is the age at which a held artifact is a defect
	// rather than a queue.
	DefaultEscalateAfter = 72 * time.Hour
)

// gateHaltReasonSuffix classifies free-form halt reasons such as
// "mayor_publish_gate" as gate holds without enumerating every variant.
const gateHaltReasonSuffix = "_gate"

// Severity ranks a gate hold for consumers that must decide whether to act.
type Severity int

const (
	// SeverityOK means the artifact is within its tolerable drift window.
	SeverityOK Severity = iota
	// SeverityWarn means the hold is aging or its provenance is incomplete.
	SeverityWarn
	// SeverityEscalate means the hold has outlived the artifact's usefulness.
	SeverityEscalate
)

// String renders the severity for log and check output.
func (s Severity) String() string {
	switch s {
	case SeverityWarn:
		return "warn"
	case SeverityEscalate:
		return "escalate"
	default:
		return "ok"
	}
}

// Thresholds are the ages at which a gate hold warns and escalates.
type Thresholds struct {
	WarnAfter     time.Duration
	EscalateAfter time.Duration
}

// DefaultThresholds returns the shipped warn/escalate ages.
func DefaultThresholds() Thresholds {
	return Thresholds{WarnAfter: DefaultWarnAfter, EscalateAfter: DefaultEscalateAfter}
}

// normalized fills unset or nonsensical values from the defaults so a
// zero-value Thresholds behaves like DefaultThresholds.
func (t Thresholds) normalized() Thresholds {
	if t.WarnAfter <= 0 {
		t.WarnAfter = DefaultWarnAfter
	}
	if t.EscalateAfter <= 0 {
		t.EscalateAfter = DefaultEscalateAfter
	}
	if t.EscalateAfter < t.WarnAfter {
		t.EscalateAfter = t.WarnAfter
	}
	return t
}

// Artifact is the gate-held work as recorded on a bead, before any git
// resolution happens.
type Artifact struct {
	BeadID     string
	Title      string
	Branch     string
	Commit     string
	Target     string
	TargetHead string
	HaltReason string
	// ReadyAt is the parsed branch_ready_at. Zero means the halting agent
	// never recorded one, which is itself a defect: the gate has no clock.
	ReadyAt time.Time
}

// ArtifactFromBead extracts gate metadata from a bead. The second return is
// false when the bead is not holding an artifact at a gate.
func ArtifactFromBead(b beads.Bead) (Artifact, bool) {
	meta := b.Metadata
	if !isGateHeld(meta) {
		return Artifact{}, false
	}
	a := Artifact{
		BeadID:     strings.TrimSpace(b.ID),
		Title:      strings.TrimSpace(b.Title),
		Branch:     metaValue(meta, MetaBranch),
		Commit:     metaValue(meta, MetaCommit),
		Target:     metaValue(meta, MetaTarget),
		TargetHead: metaValue(meta, MetaTargetHead),
		HaltReason: metaValue(meta, MetaHaltReason),
	}
	if ts, err := time.Parse(time.RFC3339, metaValue(meta, MetaBranchReadyAt)); err == nil {
		a.ReadyAt = ts.UTC()
	}
	return a, true
}

// isGateHeld reports whether metadata marks a bead as holding an artifact.
// branch_ready=true is the canonical marker; a halt_reason ending in "_gate"
// also counts so free-form gates (mayor_publish_gate, operator_gate) are not
// invisible just because the halting agent skipped the flag.
func isGateHeld(meta beads.StringMap) bool {
	if strings.EqualFold(metaValue(meta, MetaBranchReady), "true") {
		return true
	}
	return strings.HasSuffix(metaValue(meta, MetaHaltReason), gateHaltReasonSuffix)
}

func metaValue(meta beads.StringMap, key string) string {
	return strings.TrimSpace(meta[key])
}

// Resolver reads git state for an assessment. Implementations must not talk
// to a remote; see the package comment.
type Resolver interface {
	// ResolveRef returns the commit SHA a ref names, or an error when the
	// ref does not resolve locally.
	ResolveRef(ref string) (string, error)
	// CountBehind returns how many commits `to` has that `from` does not —
	// git rev-list --count from..to.
	CountBehind(from, to string) (int, error)
}

// HeadResolver additionally reports which branch the repository has checked
// out. Stamping paths need it to prove the caller is standing in the
// artifact's own worktree before recording anything derived from HEAD.
type HeadResolver interface {
	Resolver
	// CurrentBranch returns the checked-out branch name, or an error when
	// HEAD is detached or unreadable.
	CurrentBranch() (string, error)
}

// ResolveTarget returns the SHA a recorded target names. A target may be
// bare ("develop"), remote-qualified ("upstream/main"), or a raw SHA.
func ResolveTarget(r Resolver, target string) (string, error) {
	target = strings.TrimSpace(target)
	if r == nil || target == "" {
		return "", fmt.Errorf("no target to resolve")
	}
	return resolveFirst(r, targetRefCandidates(target)...)
}

// ResolveBranchTip returns the SHA a recorded branch resolves to on a
// remote — what a publisher would get by fetching that branch. A branch
// that has never been pushed does not resolve.
func ResolveBranchTip(r Resolver, branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if r == nil || branch == "" {
		return "", fmt.Errorf("no branch to resolve")
	}
	return resolveFirst(r, branchRefCandidates(branch)...)
}

// Assessment is an Artifact plus everything git could tell us about it.
type Assessment struct {
	Artifact

	// Age is how long the artifact has waited. Valid only when AgeKnown.
	Age      time.Duration
	AgeKnown bool

	// PublishSHA is the commit a publisher must actually ship, and
	// PublishSHASource names where it came from ("metadata.commit",
	// "origin/<branch>", ...). Empty when nothing resolved.
	PublishSHA       string
	PublishSHASource string

	// TargetTip is the target's current SHA as seen from local refs.
	TargetTip string

	// Behind is how many commits the target has gained that the artifact
	// does not contain. Valid only when BehindKnown.
	Behind      int
	BehindKnown bool

	// MovedSinceHalt is how far the target advanced since the recorded
	// target_head. Valid only when MovedSinceHaltKnown. It is derivable
	// without the artifact's objects, so it survives a reaped worktree.
	MovedSinceHalt      int
	MovedSinceHaltKnown bool

	// BranchTip is where metadata.branch resolves on the remote, and
	// BranchStale reports that it does not match PublishSHA — publishing
	// metadata.branch as-is would ship the wrong history. Both are valid
	// only when BranchStateKnown.
	BranchTip        string
	BranchStale      bool
	BranchStateKnown bool

	// Severity is the overall verdict; Notes explain how it was reached.
	Severity Severity
	Notes    []string
}

// Assess resolves an artifact's git state and ranks the hold. A nil
// resolver degrades to metadata-only assessment (age and provenance)
// rather than failing.
func Assess(a Artifact, r Resolver, now time.Time, t Thresholds) Assessment {
	t = t.normalized()
	out := Assessment{Artifact: a, Severity: SeverityOK}

	out.assessAge(now, t)
	out.resolveTarget(r)
	out.resolvePublishSHA(r)
	out.measureDrift(r)
	out.assessBranchAuthority(r)
	return out
}

// assessAge applies the gate clock. A missing branch_ready_at is not a
// benign omission: without it nothing can tell a fresh artifact from one
// that has waited two weeks, which is the failure this package exists to
// prevent. Treat it as at least a warning.
func (a *Assessment) assessAge(now time.Time, t Thresholds) {
	if a.ReadyAt.IsZero() {
		a.note("no %s recorded — gate age unknown (the bead's own updated_at moves on every touch and cannot stand in)", MetaBranchReadyAt)
		a.raise(SeverityWarn)
		return
	}
	a.AgeKnown = true
	a.Age = now.Sub(a.ReadyAt)
	switch {
	case a.Age >= t.EscalateAfter:
		a.raise(SeverityEscalate)
		a.note("held %s at the gate (escalate after %s)", formatAge(a.Age), formatAge(t.EscalateAfter))
	case a.Age >= t.WarnAfter:
		a.raise(SeverityWarn)
		a.note("held %s at the gate (warn after %s)", formatAge(a.Age), formatAge(t.WarnAfter))
	}
}

func (a *Assessment) resolveTarget(r Resolver) {
	if r == nil {
		return
	}
	if a.Target == "" {
		a.note("no %s recorded — cannot measure drift", MetaTarget)
		a.raise(SeverityWarn)
		return
	}
	tip, err := ResolveTarget(r, a.Target)
	if err != nil {
		a.note("target %q does not resolve locally — drift not measured (fetch the rig repo)", a.Target)
		a.raise(SeverityWarn)
		return
	}
	a.TargetTip = tip
}

// resolvePublishSHA picks the commit a publisher must ship. metadata.commit
// wins when present: it is the only field that survived the ga-b5h failure
// intact, where metadata.branch still pointed at pre-rebase history.
func (a *Assessment) resolvePublishSHA(r Resolver) {
	if r == nil {
		if a.Commit != "" {
			a.PublishSHA = a.Commit
			a.PublishSHASource = "metadata." + MetaCommit
		}
		return
	}
	if a.Commit != "" {
		if sha, err := r.ResolveRef(a.Commit); err == nil {
			a.PublishSHA = sha
			a.PublishSHASource = "metadata." + MetaCommit
			return
		}
		a.note("metadata.%s %s is not present in this repository — the worktree may have been reaped", MetaCommit, shortSHA(a.Commit))
		a.raise(SeverityWarn)
	} else {
		a.note("no metadata.%s recorded — falling back to whatever metadata.%s resolves to", MetaCommit, MetaBranch)
		a.raise(SeverityWarn)
	}
	if a.Branch == "" {
		return
	}
	for _, ref := range branchRefCandidates(a.Branch) {
		if sha, err := r.ResolveRef(ref); err == nil {
			a.PublishSHA = sha
			a.PublishSHASource = ref
			return
		}
	}
}

func (a *Assessment) measureDrift(r Resolver) {
	if r == nil || a.TargetTip == "" {
		return
	}
	if a.PublishSHA != "" {
		if behind, err := r.CountBehind(a.PublishSHA, a.TargetTip); err == nil {
			a.Behind = behind
			a.BehindKnown = true
		}
	}
	if a.TargetHead == "" {
		a.note("no %s recorded — the target SHA this was rebased onto is unknown", MetaTargetHead)
		a.raise(SeverityWarn)
		return
	}
	if moved, err := r.CountBehind(a.TargetHead, a.TargetTip); err == nil {
		a.MovedSinceHalt = moved
		a.MovedSinceHaltKnown = true
	}
}

// assessBranchAuthority answers requirement 4 of ga-qbq: metadata.branch is
// not publishable as-is when it resolves to pre-rebase history. A publisher
// following the obvious field would silently defeat the rebase, so a
// divergence is always at least a warning.
func (a *Assessment) assessBranchAuthority(r Resolver) {
	if r == nil || a.Branch == "" || a.PublishSHA == "" {
		return
	}
	// When the publish SHA was derived from the branch itself there is
	// nothing independent to compare against.
	if a.PublishSHASource != "metadata."+MetaCommit {
		return
	}
	a.BranchStateKnown = true
	sha, err := ResolveBranchTip(r, a.Branch)
	if err != nil {
		a.BranchStale = true
		a.note("metadata.%s %q is not published on any remote — only metadata.%s %s carries the artifact",
			MetaBranch, a.Branch, MetaCommit, shortSHA(a.PublishSHA))
		a.raise(SeverityWarn)
		return
	}
	a.BranchTip = sha
	if sha != a.PublishSHA {
		a.BranchStale = true
		a.note("metadata.%s %q resolves to %s on the remote, not the artifact %s — publish the commit, not the branch",
			MetaBranch, a.Branch, shortSHA(sha), shortSHA(a.PublishSHA))
		a.raise(SeverityWarn)
	}
}

func (a *Assessment) raise(s Severity) {
	if s > a.Severity {
		a.Severity = s
	}
}

func (a *Assessment) note(format string, args ...any) {
	a.Notes = append(a.Notes, fmt.Sprintf(format, args...))
}

// Line renders a one-line summary suitable for a doctor detail row.
func (a Assessment) Line() string {
	var b strings.Builder
	b.WriteString(a.BeadID)
	if a.HaltReason != "" {
		fmt.Fprintf(&b, " [%s]", a.HaltReason)
	}
	if a.AgeKnown {
		fmt.Fprintf(&b, " held %s", formatAge(a.Age))
	} else {
		b.WriteString(" held for an unknown time")
	}
	switch {
	case a.BehindKnown:
		fmt.Fprintf(&b, ", %d behind %s", a.Behind, a.targetLabel())
	case a.MovedSinceHaltKnown:
		fmt.Fprintf(&b, ", %s moved %d since rebase", a.targetLabel(), a.MovedSinceHalt)
	default:
		fmt.Fprintf(&b, ", drift vs %s unknown", a.targetLabel())
	}
	if a.PublishSHA != "" {
		fmt.Fprintf(&b, ", publish %s", shortSHA(a.PublishSHA))
	}
	if a.BranchStale {
		b.WriteString(" (metadata.branch is stale)")
	}
	if a.Title != "" {
		fmt.Fprintf(&b, " — %s", a.Title)
	}
	return b.String()
}

func (a Assessment) targetLabel() string {
	if a.Target == "" {
		return "target"
	}
	return a.Target
}

// targetRefCandidates orders the refs a recorded target may name.
// "upstream/main" must resolve as refs/remotes/upstream/main, while a bare
// "develop" means origin's copy; a raw SHA resolves last.
func targetRefCandidates(target string) []string {
	candidates := make([]string, 0, 3)
	if strings.Contains(target, "/") {
		candidates = append(candidates, "refs/remotes/"+target)
	}
	candidates = append(candidates, "refs/remotes/origin/"+target, target)
	return candidates
}

// branchRefCandidates orders the remote refs a recorded branch may name.
// Only remote refs are consulted: the question this answers is "what would
// a publisher fetch?", and a local ref answers a different question.
func branchRefCandidates(branch string) []string {
	candidates := make([]string, 0, 2)
	if strings.Contains(branch, "/") {
		// A branch such as "polecat/ga-qbq" is far more likely to live on
		// origin than to name the "polecat" remote, so try origin first and
		// let the remote-qualified reading be the fallback.
		candidates = append(candidates, "refs/remotes/origin/"+branch, "refs/remotes/"+branch)
		return candidates
	}
	return append(candidates, "refs/remotes/origin/"+branch)
}

func resolveFirst(r Resolver, refs ...string) (string, error) {
	var lastErr error
	for _, ref := range refs {
		sha, err := r.ResolveRef(ref)
		if err == nil {
			return sha, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no refs supplied")
	}
	return "", lastErr
}

func shortSHA(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}

// formatAge renders a duration the way an operator reads a gate age:
// days and hours once it is past a day, hours and minutes below that.
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= 24*time.Hour {
		days := int(d / (24 * time.Hour))
		hours := int((d % (24 * time.Hour)) / time.Hour)
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	if d >= time.Hour {
		hours := int(d / time.Hour)
		mins := int((d % time.Hour) / time.Minute)
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}
