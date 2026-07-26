package main

// Publish-gate provenance stamping (ga-qbq).
//
// When an agent halts at a publish gate it writes `--set-metadata
// branch_ready=true` and walks away. Everything a later reader needs to
// judge that artifact — when it arrived, which target SHA it was verified
// against, whether metadata.branch still points at the artifact — was left
// to prose in each formula's done sequence, and the prose did not say it.
// ga-b5h sat fifteen days with no arrival time, no target head, and a
// metadata.branch that resolved on origin to pre-rebase history; a
// publisher following the obvious field would have shipped the wrong SHA.
//
// Stamping the provenance here makes it a property of the write rather than
// of whichever formula text happened to be deployed. It is deliberately
// conservative: it only augments a write that is already declaring
// branch_ready=true, it never overwrites a key the caller set itself, and
// every git-derived key is gated on proof that the caller is standing in
// the artifact's own worktree. Any doubt and the key is simply omitted —
// a halt must never fail because provenance could not be resolved.

import (
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/publishgate"
)

// publishGateStampNow supplies the branch_ready_at instant. Package var so
// tests can pin it; the value is normalized to UTC either way.
var publishGateStampNow = time.Now

// newPublishGateStampResolver opens the repository the stamp reads. Package
// var so tests can substitute a fake without a real worktree.
var newPublishGateStampResolver = func(dir string) publishgate.HeadResolver {
	return publishgate.NewGitResolver(dir)
}

// stampPublishGateArgs augments a `bd update ... --set-metadata
// branch_ready=true` invocation with publish-gate provenance. Every other
// invocation passes through untouched.
//
// existing lazily supplies the bead's current metadata; it is called only
// once the write is known to be a branch-ready halt, so ordinary `gc bd`
// traffic never pays for the lookup. It may return nil.
func stampPublishGateArgs(bdArgs []string, repoDir string, existing func() beads.StringMap) []string {
	if len(bdArgs) == 0 || bdArgs[0] != "update" {
		return bdArgs
	}
	// A bare "--" turns everything after it into positionals, so appended
	// flags would reach bd as bead IDs.
	for _, arg := range bdArgs {
		if arg == "--" {
			return bdArgs
		}
	}
	edits, err := parseWorkRecordMetadataEdits(bdArgs)
	if err != nil || len(edits.setMetadata) == 0 {
		return bdArgs
	}
	// bd rejects --metadata combined with --set-metadata, so appending our
	// own edits to a JSON-form write would break the caller's command.
	if edits.hasMetadataJSON {
		return bdArgs
	}
	writing := beads.StringMap{}
	if applyErr := applyWorkRecordMetadataEdits(writing, edits); applyErr != nil {
		return bdArgs
	}
	if !strings.EqualFold(strings.TrimSpace(writing[publishgate.MetaBranchReady]), "true") {
		return bdArgs
	}

	var current beads.StringMap
	if existing != nil {
		current = existing()
	}
	additions := publishGateStampAdditions(writing, current, repoDir)
	if len(additions) == 0 {
		return bdArgs
	}
	keys := make([]string, 0, len(additions))
	for k := range additions {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(bdArgs)+2*len(keys))
	out = append(out, bdArgs...)
	for _, k := range keys {
		out = append(out, "--set-metadata", k+"="+additions[k])
	}
	return out
}

// publishGateStampAdditions computes the provenance keys missing from a
// branch-ready write. writing holds the metadata the caller is already
// setting and always wins; existing is the bead's current metadata, used
// only to decide whether the gate clock is already running.
func publishGateStampAdditions(writing, existing beads.StringMap, repoDir string) map[string]string {
	additions := make(map[string]string, 4)
	commit := strings.TrimSpace(writing[publishgate.MetaCommit])

	if resolver := publishGateWorktreeResolver(writing, repoDir); resolver != nil {
		if commit == "" {
			if head, err := resolver.ResolveRef("HEAD"); err == nil {
				commit = head
				additions[publishgate.MetaCommit] = head
			}
		}
		if _, ok := writing[publishgate.MetaTargetHead]; !ok {
			if tip, err := publishgate.ResolveTarget(resolver, writing[publishgate.MetaTarget]); err == nil {
				additions[publishgate.MetaTargetHead] = tip
			}
		}
		// branch_stale answers "is metadata.branch publishable as-is?" at
		// the moment of the halt. An unpushed or pre-rebase branch is stale:
		// the artifact lives at metadata.commit and nowhere else.
		if _, ok := writing[publishgate.MetaBranchStale]; !ok && commit != "" {
			branch := strings.TrimSpace(writing[publishgate.MetaBranch])
			tip, err := publishgate.ResolveBranchTip(resolver, branch)
			additions[publishgate.MetaBranchStale] = boolMetadataValue(err != nil || tip != commit)
		}
	}

	if _, ok := writing[publishgate.MetaBranchReadyAt]; !ok && publishGateClockShouldStart(existing, commit) {
		additions[publishgate.MetaBranchReadyAt] = publishGateStampNow().UTC().Format(time.RFC3339)
	}
	return additions
}

// publishGateWorktreeResolver returns a repository reader only when the
// caller is provably standing in the artifact's own worktree: it must be on
// the branch it is recording. Without that proof nothing derived from HEAD
// describes the artifact, so the caller gets no git-derived provenance.
func publishGateWorktreeResolver(writing beads.StringMap, repoDir string) publishgate.HeadResolver {
	branch := strings.TrimSpace(writing[publishgate.MetaBranch])
	if branch == "" || strings.TrimSpace(repoDir) == "" || newPublishGateStampResolver == nil {
		return nil
	}
	resolver := newPublishGateStampResolver(repoDir)
	if resolver == nil {
		return nil
	}
	if current, err := resolver.CurrentBranch(); err != nil || current != branch {
		return nil
	}
	return resolver
}

// publishGateClockShouldStart reports whether a fresh branch_ready_at is
// warranted. A running clock is only reset by a genuinely new artifact:
// re-running a halt sequence after a crash, or anyone else re-marking the
// bead, must not make a two-week-old wait read as brand new — that
// invisibility is the whole failure ga-qbq was filed about. When the caller
// cannot prove which commit it is halting, the existing clock always wins.
func publishGateClockShouldStart(existing beads.StringMap, commit string) bool {
	previous := strings.TrimSpace(existing[publishgate.MetaBranchReadyAt])
	if previous == "" {
		return true
	}
	priorCommit := strings.TrimSpace(existing[publishgate.MetaCommit])
	if commit == "" || priorCommit == "" {
		return false
	}
	return priorCommit != commit
}

func boolMetadataValue(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// publishGateStampRepoDir returns the directory the stamp resolves refs in:
// the caller's own working directory, which for a worker is its worktree.
func publishGateStampRepoDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// publishGateExistingMetadata reads the current metadata of the single bead
// a write targets. Every failure — ambiguous args, a batch write, an
// unreachable store, a bead bd has not projected yet — returns nil, which
// the stamp treats as "no clock running". It must never block the write.
func publishGateExistingMetadata(cityPath string, target execStoreTarget, bdArgs []string) beads.StringMap {
	ids, ok, ambiguous := bdMutationWriteIDs(bdArgs)
	if !ok || ambiguous || len(ids) != 1 {
		return nil
	}
	store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
	if err != nil {
		return nil
	}
	bead, err := store.Get(ids[0])
	if err != nil {
		return nil
	}
	return bead.Metadata
}
