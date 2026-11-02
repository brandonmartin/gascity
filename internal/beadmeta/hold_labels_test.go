package beadmeta

import "testing"

// TestDispatchHoldLabelsMatchCanonicalHoldValues pins beadmeta as the single
// named home for the two canonical hold values documented in
// engdocs/contributors/hold-label-conventions.md (hold:mayor, hold:external)
// so internal/config and cmd/gc can share one definition instead of each
// re-spelling the label strings (ga-x9kptu / ga-5736js).
func TestDispatchHoldLabelsMatchCanonicalHoldValues(t *testing.T) {
	if HoldMayorLabel != "hold:mayor" {
		t.Fatalf("HoldMayorLabel = %q, want %q", HoldMayorLabel, "hold:mayor")
	}
	if HoldExternalLabel != "hold:external" {
		t.Fatalf("HoldExternalLabel = %q, want %q", HoldExternalLabel, "hold:external")
	}
	want := []string{HoldMayorLabel, HoldExternalLabel}
	if len(DispatchHoldLabels) != len(want) {
		t.Fatalf("DispatchHoldLabels = %#v, want %#v", DispatchHoldLabels, want)
	}
	for i, v := range want {
		if DispatchHoldLabels[i] != v {
			t.Fatalf("DispatchHoldLabels[%d] = %q, want %q", i, DispatchHoldLabels[i], v)
		}
	}
}

func TestHasDispatchHold(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"nil", nil, false},
		{"unrelated", []string{"mysql-cutover", "needs-mayor", "mpr-human-hold"}, false},
		{"other hold value", []string{"hold:polecat"}, true},
		{"hold namespace suffix", []string{"hold:mayor-go"}, true},
		{"case folded hold namespace", []string{" Hold:Mayor-Go "}, true},
		{"hold external", []string{"x", HoldExternalLabel}, true},
		{"hold mayor", []string{HoldMayorLabel}, true},
		{"case and space folded", []string{"  Hold:External "}, true},
		{"hold-like unrelated labels", []string{"holding-pattern", "hold", "needs-mayor"}, false},
		// bd set-state <id> hold=cleared releases a hold and leaves hold:cleared
		// behind (ga-azdbu), so that label is "no hold", not a parked bead.
		{"released hold", []string{HoldClearedLabel}, false},
		{"released hold case and space folded", []string{"  Hold:Cleared "}, false},
		{"released hold beside unrelated label", []string{"mysql-cutover", HoldClearedLabel}, false},
		// A hold added after the release sits beside hold:cleared on either side
		// and must still park the bead.
		{"released then re-held", []string{HoldClearedLabel, HoldMayorLabel}, true},
		{"re-held then released label", []string{HoldMayorLabel, HoldClearedLabel}, true},
		{"re-held with namespaced reason", []string{HoldClearedLabel, "hold:mayor-go"}, true},
		// Only the exact released label is exempt; a longer suffix is a hold.
		{"released-label lookalike", []string{"hold:cleared-by-mayor"}, true},
	}
	for _, tc := range cases {
		if got := HasDispatchHold(tc.labels); got != tc.want {
			t.Errorf("%s: HasDispatchHold(%q) = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
}

// TestDispatchHoldReleasedLabelsAreOutsideTheCanonicalHolds pins hold:cleared as
// the one label in the reserved hold: namespace that is not a hold: the label bd
// leaves behind when `bd set-state <id> hold=cleared` releases a hold. It must
// not join DispatchHoldLabels, which bd's exact --exclude-label flags render as
// holds (ga-azdbu).
func TestDispatchHoldReleasedLabelsAreOutsideTheCanonicalHolds(t *testing.T) {
	if HoldClearedLabel != "hold:cleared" {
		t.Fatalf("HoldClearedLabel = %q, want %q", HoldClearedLabel, "hold:cleared")
	}
	if len(DispatchHoldReleasedLabels) != 1 || DispatchHoldReleasedLabels[0] != HoldClearedLabel {
		t.Fatalf("DispatchHoldReleasedLabels = %#v, want [%q]", DispatchHoldReleasedLabels, HoldClearedLabel)
	}
	for _, held := range DispatchHoldLabels {
		for _, released := range DispatchHoldReleasedLabels {
			if held == released {
				t.Errorf("DispatchHoldLabels names %q, which is a released label", held)
			}
		}
	}
}
