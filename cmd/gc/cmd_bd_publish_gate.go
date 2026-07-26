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
func stampPublishGateArgs(bdArgs []string, repoDir string) []string {
	if len(bdArgs) == 0 || bdArgs[0] != "update" {
		return bdArgs
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

	additions := publishGateStampAdditions(writing, repoDir)
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
// setting; keys present there are never overridden.
func publishGateStampAdditions(writing beads.StringMap, repoDir string) map[string]string {
	additions := make(map[string]string, 4)
	if _, ok := writing[publishgate.MetaBranchReadyAt]; !ok {
		additions[publishgate.MetaBranchReadyAt] = publishGateStampNow().UTC().Format(time.RFC3339)
	}

	branch := strings.TrimSpace(writing[publishgate.MetaBranch])
	if branch == "" || strings.TrimSpace(repoDir) == "" || newPublishGateStampResolver == nil {
		// Without a recorded branch there is nothing to prove the caller is
		// in the artifact's worktree, so no git-derived key is trustworthy.
		return additions
	}
	resolver := newPublishGateStampResolver(repoDir)
	if resolver == nil {
		return additions
	}
	// The worktree proof: a halting agent records the branch it is standing
	// on. Anyone else touching the bead from another directory fails here
	// and gets the timestamp only.
	if current, err := resolver.CurrentBranch(); err != nil || current != branch {
		return additions
	}

	commit := strings.TrimSpace(writing[publishgate.MetaCommit])
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
	// branch_stale answers "is metadata.branch publishable as-is?" at the
	// moment of the halt. An unpushed or pre-rebase branch is stale: the
	// artifact lives at metadata.commit and nowhere else.
	if _, ok := writing[publishgate.MetaBranchStale]; !ok && commit != "" {
		tip, err := publishgate.ResolveBranchTip(resolver, branch)
		additions[publishgate.MetaBranchStale] = boolMetadataValue(err != nil || tip != commit)
	}
	return additions
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
