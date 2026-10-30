package beads

import (
	"fmt"
	"testing"
)

func TestWispResidencyHintsStayBounded(t *testing.T) {
	var h wispResidencyHints
	for i := 0; i < wispResidencyHintCap; i++ {
		h.note(fmt.Sprintf("gc-wisp-%d", i))
	}
	if got := h.len(); got != wispResidencyHintCap {
		t.Fatalf("hints = %d, want %d", got, wispResidencyHintCap)
	}
	h.note("gc-wisp-overflow")
	if got := h.len(); got != 1 || !h.has("gc-wisp-overflow") {
		t.Fatalf("after overflow: hints = %d, has newest = %v; want only the newest", got, h.has("gc-wisp-overflow"))
	}
	h.forget("gc-wisp-overflow")
	if h.has("gc-wisp-overflow") {
		t.Fatal("forgotten hint still present")
	}
}
