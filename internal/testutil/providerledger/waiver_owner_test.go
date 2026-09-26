package providerledger

import (
	"regexp"
	"testing"
)

// waiverOwnerPattern is the shape of a bead id: a prefix, a suffix, and an
// optional child index. It is deliberately not a liveness check — resolving a
// bead needs a bd shell-out, which would itself need a waiver from this ledger.
var waiverOwnerPattern = regexp.MustCompile(`^[a-z]{2,4}-[a-z0-9]+(\.[0-9]+)*$`)

// TestCatalogHasNoRuntimeWaivers ratchets the runtime.Provider ledger at zero
// waivers.
//
// ga-p20 retired the last dated runtime waivers by proving every production
// constructor (or its seam, with the unproved residue named in the proof
// scope). Three successive date bumps preceded that, each renewing the same
// gaps without a contract landing. A new waiver therefore has to be a
// deliberate, reviewed edit of this test — naming its owner bead and why the
// constructor cannot be proved — never a silent line added to Catalog().
func TestCatalogHasNoRuntimeWaivers(t *testing.T) {
	for _, entry := range Catalog() {
		for _, claim := range entry.Claims {
			if claim.Contract != ContractRuntimeProvider {
				continue
			}
			if claim.Disposition == DispositionWaived || claim.Waiver != nil {
				t.Errorf("%s: %s is waived; every runtime.Provider constructor is proved or not applicable since ga-p20, so a new waiver needs an explicit edit to this ratchet",
					entry.ID, renderSymbolRef(claim.Constructor))
			}
		}
	}
}

// TestEveryWaiverNamesAnOwner checks that no catalog waiver is left unowned.
//
// Validate reports an empty owner as a structural problem, but only when the
// waiver is otherwise well formed enough to be reached; this asserts it
// directly against the shipped catalog so an unowned waiver cannot ride in
// behind an unrelated failure.
func TestEveryWaiverNamesAnOwner(t *testing.T) {
	for _, entry := range Catalog() {
		for _, claim := range entry.Claims {
			if claim.Waiver == nil {
				continue
			}
			if !waiverOwnerPattern.MatchString(claim.Waiver.Owner) {
				t.Errorf("%s %s: waiver owner = %q, want a bead id", entry.ID, claim.Contract, claim.Waiver.Owner)
			}
		}
	}
}
