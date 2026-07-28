package convoy

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// ConvoyDeps bundles dependencies for convoy operations.
type ConvoyDeps struct {
	Cfg       *config.City
	GetStore  func(rig string) (beads.Store, error)
	FindStore func(beadID string) (beads.Store, error)
	Recorder  events.Recorder
}

// ConvoyCreateInput holds the parameters for creating a convoy.
type ConvoyCreateInput struct {
	Title  string
	Items  []string
	Fields ConvoyFields
	Labels []string
}

// ConvoyCreateResult holds the result of creating a convoy.
type ConvoyCreateResult struct {
	Convoy      beads.Bead
	LinkedCount int
}

// ConvoyProgressResult holds the progress of a convoy.
type ConvoyProgressResult struct {
	ConvoyID string
	Total    int
	Closed   int
	Complete bool
}

// ConvoyCreate creates a convoy bead, applies metadata, links child items,
// and emits a ConvoyCreated event.
//
// The convoy bead is created in store and its tracks edges are written to
// store. The linked items may live in different per-class stores; memberStores
// supplies the additional stores to probe when verifying each linked item
// exists. On origin/main the one store collapses memberStores to its empty
// default, reproducing today's single-store linking exactly.
func ConvoyCreate(deps ConvoyDeps, store beads.Store, input ConvoyCreateInput, memberStores ...beads.Store) (ConvoyCreateResult, error) {
	b := beads.Bead{
		Title:  input.Title,
		Type:   "convoy",
		Labels: input.Labels,
	}
	ApplyConvoyFields(&b, input.Fields)

	convoy, err := store.Create(b)
	if err != nil {
		return ConvoyCreateResult{}, fmt.Errorf("creating convoy: %w", err)
	}

	linked := 0
	for _, itemID := range input.Items {
		if err := TrackItem(store, convoy.ID, itemID, memberStores...); err != nil {
			return ConvoyCreateResult{Convoy: convoy, LinkedCount: linked},
				fmt.Errorf("linking item %s: %w", itemID, err)
		}
		linked++
	}

	if deps.Recorder != nil {
		deps.Recorder.Record(events.Event{
			Type:    events.ConvoyCreated,
			Subject: convoy.ID,
		})
	}

	return ConvoyCreateResult{Convoy: convoy, LinkedCount: linked}, nil
}

// ConvoyProgress returns the completion progress of a convoy.
//
// The convoy bead is read from store; its tracked members may live in different
// per-class stores, so memberStores supplies the additional stores Members
// probes. On origin/main the one store collapses memberStores to its empty
// default and progress is computed over the single store exactly as today.
func ConvoyProgress(_ ConvoyDeps, store beads.Store, id string, memberStores ...beads.Store) (ConvoyProgressResult, error) {
	b, err := store.Get(id)
	if err != nil {
		return ConvoyProgressResult{}, fmt.Errorf("getting convoy %s: %w", id, err)
	}
	if b.Type != "convoy" {
		return ConvoyProgressResult{}, fmt.Errorf("bead %s is not a convoy (type: %s)", id, b.Type)
	}

	children, err := Members(store, id, true, memberStores...)
	if err != nil {
		return ConvoyProgressResult{}, fmt.Errorf("listing tracked items of %s: %w", id, err)
	}

	total := len(children)
	closed := 0
	for _, c := range children {
		if IsTerminalStatus(c.Status) {
			closed++
		}
	}

	return ConvoyProgressResult{
		ConvoyID: id,
		Total:    total,
		Closed:   closed,
		Complete: total > 0 && closed == total,
	}, nil
}

// ConvoyAddItems links beads to an existing convoy.
//
// The convoy bead and the new tracks edges are read from and written to store.
// The linked items may live in different per-class stores; memberStores
// supplies the additional stores to probe when verifying each item exists. On
// origin/main the one store collapses memberStores to its empty default.
func ConvoyAddItems(_ ConvoyDeps, store beads.Store, convoyID string, items []string, memberStores ...beads.Store) error {
	b, err := store.Get(convoyID)
	if err != nil {
		return fmt.Errorf("getting convoy %s: %w", convoyID, err)
	}
	if b.Type != "convoy" {
		return fmt.Errorf("bead %s is not a convoy (type: %s)", convoyID, b.Type)
	}

	for _, itemID := range items {
		if err := TrackItem(store, convoyID, itemID, memberStores...); err != nil {
			return fmt.Errorf("linking item %s to convoy %s: %w", itemID, convoyID, err)
		}
	}
	return nil
}

// explicitReasonCloser is implemented by stores whose close path accepts a
// reason directly (BdStore maps it to `bd close --reason ...`).
type explicitReasonCloser interface {
	CloseWithReason(id, reason string) error
}

// CloseWithReason stamps a close_reason metadata key on a convoy bead before
// closing it. Stores that accept a reason on the close call receive the same
// text, which lets cities running with validation.on-close=error accept
// system-driven closes whose default reason ("Closed") would otherwise be
// rejected as terse. For stores whose Close path does not consult the reason,
// the metadata still serves as a permanent audit trail of why the convoy was
// closed.
//
// An empty or whitespace-only reason falls through to a plain Close rather
// than stamping a meaningless value that downstream validators would trip on.
func CloseWithReason(store beads.Store, id, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return store.Close(id)
	}
	if err := store.SetMetadata(id, "close_reason", reason); err != nil {
		return fmt.Errorf("stamping convoy %s close reason: %w", id, err)
	}
	if closer, ok := store.(explicitReasonCloser); ok {
		return closer.CloseWithReason(id, reason)
	}
	return store.Close(id)
}

// ConvoyClose closes a convoy bead and emits a ConvoyClosed event.
func ConvoyClose(deps ConvoyDeps, store beads.Store, id string) error {
	if _, err := store.Get(id); err != nil {
		return fmt.Errorf("getting convoy %s: %w", id, err)
	}

	if err := store.Close(id); err != nil {
		return fmt.Errorf("closing convoy %s: %w", id, err)
	}

	if deps.Recorder != nil {
		deps.Recorder.Record(events.Event{
			Type:    events.ConvoyClosed,
			Subject: id,
		})
	}

	return nil
}
