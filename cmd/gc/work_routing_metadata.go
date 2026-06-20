package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func legacyWorkflowRunTarget(b beads.Bead) string {
	if strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]) != "workflow" {
		return ""
	}
	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
		return ""
	}
	return strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey])
}

func routedToOrLegacyWorkflowTarget(b beads.Bead) string {
	if routedTo := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]); routedTo != "" {
		return routedTo
	}
	return legacyWorkflowRunTarget(b)
}

// consumedPoolRouteAnchor returns the pool route retained as the gc.run_target
// anchor after gc.routed_to was consumed on claim (ga-sa0), or "" when the bead
// carries no recoverable anchor. It is the orphan-recovery counterpart to
// claimConsumingRoute: clearing gc.routed_to on claim would otherwise strand a
// dead owner's in-progress pool work, so releaseOrphanedPoolAssignments falls
// back to this anchor to reopen that work (and re-stamp gc.routed_to from it).
//
// It deliberately recovers only a plain (kind-less) pool work bead. A bead that
// still carries gc.routed_to is already routed and yields ""; any non-empty
// gc.kind is a control-dispatcher or workflow-topology construct whose
// gc.run_target is a dispatch/structure target an agent never claims from a
// pool, so restoring gc.routed_to on one would mis-route it into pool demand.
// The lone claimable kind, "workflow", is already handled upstream of this
// fallback by routedToOrLegacyWorkflowTarget/legacyWorkflowRunTarget. The choice
// is judgment-free: it only copies a route the bead already declares.
func consumedPoolRouteAnchor(b beads.Bead) string {
	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
		return ""
	}
	if strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]) != "" {
		return ""
	}
	return strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey])
}

func routedToAndLegacyWorkflowCandidates(b beads.Bead) []string {
	routedTo := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
	legacy := legacyWorkflowRunTarget(b)
	if routedTo == "" {
		if legacy == "" {
			return nil
		}
		return []string{legacy}
	}
	return []string{routedTo}
}
