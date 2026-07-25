package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// sweepDetachedHandoffOrphans restores gc.routed_to on work beads that were
// fully detached by a failed done sequence. When a worker clears both the
// assignee and gc.routed_to in a single atomic update (e.g. because
// $REFINERY_TARGET resolved empty), the bead is left open+unassigned+unrouted
// with a branch already on origin — invisible to both the pool demand probe
// (which keys on gc.routed_to) and releaseOrphanedPoolAssignments (which only
// processes assigned work). This sweep finds such beads via gc.session_name →
// session bead → template and re-stamps gc.routed_to, returning each bead to
// pool demand. Returns the count of restored beads.
//
// Recovery is judgment-free (ZFC): it reads the original pool route from the
// session bead's own template metadata and re-stamps gc.routed_to. The bead
// re-enters pool demand; the formula re-evaluates it from there. No role names
// appear in this function.
func sweepDetachedHandoffOrphans(store beads.Store) (int, error) {
	if store == nil {
		return 0, nil
	}
	// Scan open beads for detached handoff orphans. In steady state there are
	// none, so the candidate slice is empty and the expensive session-index
	// lookup is skipped entirely.
	items, err := store.List(beads.ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		return 0, fmt.Errorf("listing open beads: %w", err)
	}

	type candidate struct {
		id, sessionName string
	}
	var candidates []candidate
	for _, b := range items {
		if !isDetachedHandoffOrphanCandidate(b) {
			continue
		}
		candidates = append(candidates, candidate{
			id:          b.ID,
			sessionName: strings.TrimSpace(b.Metadata[beadmeta.SessionNameMetadataKey]),
		})
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	// Build a session_name → pool-route index once for all candidates.
	routeIndex, indexErr := buildDetachedOrphanRouteIndex(store)
	if indexErr != nil {
		return 0, fmt.Errorf("building session route index: %w", indexErr)
	}

	var (
		restored int
		errs     []error
	)
	for _, c := range candidates {
		route := routeIndex[c.sessionName]
		if route == "" {
			log.Printf("sweepDetachedHandoffOrphans: no recoverable route for bead %s (gc.session_name=%q not found in any session bead or session carries no template)", c.id, c.sessionName)
			continue
		}
		if setErr := store.SetMetadata(c.id, beadmeta.RoutedToMetadataKey, route); setErr != nil {
			errs = append(errs, fmt.Errorf("bead %s: restoring gc.routed_to=%q: %w", c.id, route, setErr))
			continue
		}
		log.Printf("sweepDetachedHandoffOrphans: restored gc.routed_to=%q on detached handoff orphan %s", route, c.id)
		restored++
	}
	return restored, errors.Join(errs...)
}

// isDetachedHandoffOrphanCandidate reports whether b has the signature of a
// fully-detached handoff orphan: open, unassigned, no pool route, branch set
// (indicating work was done and pushed), and a session reference from which the
// pool route can be recovered.
func isDetachedHandoffOrphanCandidate(b beads.Bead) bool {
	if b.Status != "open" {
		return false
	}
	if strings.TrimSpace(b.Assignee) != "" {
		return false // still assigned — releaseOrphanedPoolAssignments covers this path
	}
	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
		return false // already has a pool route
	}
	if strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]) == beadmeta.KindWorkflow {
		return false // workflow roots use gc.run_target; restoreCarriedWorkRoutes handles them
	}
	if strings.TrimSpace(b.Metadata["branch"]) == "" {
		return false // no branch → not a completed-work handoff bead
	}
	return strings.TrimSpace(b.Metadata[beadmeta.SessionNameMetadataKey]) != ""
}

// buildDetachedOrphanRouteIndex returns a map from session_name to pool route
// for every session bead (open or closed) that carries a template. Closed
// session beads are included because the worker session is typically gone by
// the time this sweep runs.
func buildDetachedOrphanRouteIndex(store beads.Store) (map[string]string, error) {
	// Routed through the sessions class front door rather than reading session
	// beads raw, per the typed-class edge guard.
	all, listErr := sessionFrontDoor(store).ListAll(session.ListAllOptions{IncludeClosed: true})
	// Hard errors return nil rows; surface them to the caller.
	// Partial results still yield rows we can use — don't propagate.
	if listErr != nil && !beads.IsPartialResult(listErr) {
		return nil, fmt.Errorf("listing session beads: %w", listErr)
	}
	index := make(map[string]string, len(all))
	for _, si := range all {
		// SessionNameMetadata is the RAW session_name, matching the previous
		// bead-level read. Info.SessionName must NOT be used here: it applies a
		// sessionNameFor(ID) fallback that would synthesize a name for every
		// session bead and defeat the empty-name skip below.
		sn := strings.TrimSpace(si.SessionNameMetadata)
		if sn == "" {
			continue
		}
		if _, exists := index[sn]; exists {
			continue // keep first match; duplicate session names are rare edge cases
		}
		if route := retiredSessionFallbackRouteInfo(si); route != "" {
			index[sn] = route
		}
	}
	return index, nil
}

// sweepDetachedHandoffOrphansAcrossStores sweeps for fully-detached handoff
// orphans across the city store and every active rig store. Errors are logged
// to stderr; per-store failures do not abort remaining stores. Returns the
// total count of beads whose gc.routed_to was restored.
func sweepDetachedHandoffOrphansAcrossStores(cityStore beads.Store, rigStores map[string]beads.Store, logPrefix string, stderr io.Writer) int {
	if stderr == nil {
		stderr = io.Discard
	}
	type scope struct {
		label string
		store beads.Store
	}
	scopes := []scope{{label: "city", store: cityStore}}
	for name, s := range rigStores {
		scopes = append(scopes, scope{label: "rig " + name, store: s})
	}
	total := 0
	for _, sc := range scopes {
		if sc.store == nil {
			continue
		}
		n, err := sweepDetachedHandoffOrphans(sc.store)
		if err != nil {
			fmt.Fprintf(stderr, "%s: detached handoff orphan sweep (%s): %v\n", logPrefix, sc.label, err) //nolint:errcheck
		}
		if n > 0 {
			fmt.Fprintf(stderr, "%s: detached handoff orphan sweep (%s): restored gc.routed_to on %d bead(s)\n", logPrefix, sc.label, n) //nolint:errcheck
		}
		total += n
	}
	return total
}
