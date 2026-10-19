package beadmeta

import (
	"slices"
	"testing"
)

func TestExcludeClaimantsMetadataKeyPinned(t *testing.T) {
	if ExcludeClaimantsMetadataKey != "gc.exclude_claimants" {
		t.Fatalf("ExcludeClaimantsMetadataKey = %q, want %q", ExcludeClaimantsMetadataKey, "gc.exclude_claimants")
	}
	if !slices.Contains(KnownMetadataKeys, ExcludeClaimantsMetadataKey) {
		t.Fatalf("ExcludeClaimantsMetadataKey is not registered in KnownMetadataKeys")
	}
}

func TestExcludedClaimants(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  []string
	}{
		{"empty", "", nil},
		{"only separators and space", " , ,, ", nil},
		{"one", "helios/review.one", []string{"helios/review.one"}},
		{"trimmed list", " helios/review.one , helios/review.two,", []string{"helios/review.one", "helios/review.two"}},
		{"tabs and newlines trimmed", "\thelios/review.one\n,helios/review.two ", []string{"helios/review.one", "helios/review.two"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExcludedClaimants(tc.value); !slices.Equal(got, tc.want) {
				t.Fatalf("ExcludedClaimants(%q) = %#v, want %#v", tc.value, got, tc.want)
			}
		})
	}
}

func TestExcludesClaimant(t *testing.T) {
	md := map[string]string{ExcludeClaimantsMetadataKey: "helios/review.one, helios/review.two"}
	cases := []struct {
		name       string
		metadata   map[string]string
		identities []string
		want       bool
	}{
		{"nil metadata", nil, []string{"helios/review.one"}, false},
		{"no key", map[string]string{"gc.routed_to": "helios/review"}, []string{"helios/review.one"}, false},
		{"no identities", md, nil, false},
		{"empty identity never matches", map[string]string{ExcludeClaimantsMetadataKey: ","}, []string{"", " "}, false},
		{"listed identity", md, []string{"sess-1", "helios/review.two"}, true},
		{"identity is trimmed", md, []string{" helios/review.one "}, true},
		{"unlisted identity", md, []string{"helios/review.three"}, false},
		{"pool name is not a member", md, []string{"helios/review"}, false},
		{"case-sensitive like route identities", md, []string{"Helios/Review.One"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExcludesClaimant(tc.metadata, tc.identities...); got != tc.want {
				t.Fatalf("ExcludesClaimant(%v, %v) = %v, want %v", tc.metadata, tc.identities, got, tc.want)
			}
		})
	}
}
