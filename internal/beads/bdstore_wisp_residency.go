package beads

import "sync"

// wispResidencyHintCap bounds the IDs one store remembers as wisp-tier rows.
// A full set is dropped rather than evicted piecemeal: the hint only orders
// Get's two reads, so losing it costs one extra bd show per remembered row.
const wispResidencyHintCap = 4096

// wispResidencyHints remembers the IDs whose last Get this store answered
// from the wisp tier. bd show reads only the issues table, so a Get of a wisp
// row always pays a bd show miss before the wisp-tier query finds it. Session
// rows are wisps and the reconciler reads them many times a tick, so that
// miss is one forked bd process per read. Get asks the wisp tier first for a
// remembered ID and keeps the full two-tier read behind a miss there.
//
// When an ID has rows in both tiers, the hinted read answers with the wisp
// row where the full read answers with the issues row. List already prefers
// the wisp row on such a duplicate (mergeListTierResults), so a hinted Get
// agrees with List.
type wispResidencyHints struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func (h *wispResidencyHints) has(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.ids[id]
	return ok
}

func (h *wispResidencyHints) note(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ids == nil || len(h.ids) >= wispResidencyHintCap {
		h.ids = make(map[string]struct{})
	}
	h.ids[id] = struct{}{}
}

func (h *wispResidencyHints) forget(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.ids, id)
}

func (h *wispResidencyHints) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ids)
}

// getHintedWisp is Get's first read for an ID remembered as a wisp-tier row.
// found reports the row; checked reports that the wisp tier answered cleanly
// without it, so the full read need not ask that tier again. A failed query
// proves nothing and leaves both false. Any answer but found drops the hint.
func (s *BdStore) getHintedWisp(id string) (b Bead, found, checked bool) {
	wisps, err := s.getEphemeralByID(id)
	if err == nil {
		for _, w := range wisps {
			if w.ID == id {
				return w, true, true
			}
		}
	}
	s.wispHints.forget(id)
	return Bead{}, false, err == nil
}
