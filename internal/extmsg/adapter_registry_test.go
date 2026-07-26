package extmsg

import "testing"

func TestAdapterRegistry_HasProvider(t *testing.T) {
	reg := NewAdapterRegistry()
	ref := testConversationRef()

	if reg.HasProvider(ref.Provider) {
		t.Fatalf("HasProvider(%q) on empty registry = true, want false", ref.Provider)
	}

	reg.Register(AdapterKey{Provider: ref.Provider, AccountID: ref.AccountID}, newStubAdapter("stub", ref))

	if !reg.HasProvider(ref.Provider) {
		t.Fatalf("HasProvider(%q) after Register = false, want true", ref.Provider)
	}
	if reg.HasProvider("slack") {
		t.Fatalf("HasProvider(\"slack\") = true, want false for an unregistered provider")
	}
}

// TestAdapterRegistry_HasProviderIgnoresAccountID documents that provider
// wiring is account-independent: a provider counts as wired as soon as any
// one of its accounts has an adapter. Outbound diagnostics need this
// granularity because a caller can name a conversation whose AccountID it
// never learned, which would make an exact-key lookup falsely report the
// whole provider as unwired.
func TestAdapterRegistry_HasProviderIgnoresAccountID(t *testing.T) {
	reg := NewAdapterRegistry()
	ref := testConversationRef()
	reg.Register(AdapterKey{Provider: ref.Provider, AccountID: "some-other-account"}, newStubAdapter("stub", ref))

	if !reg.HasProvider(ref.Provider) {
		t.Fatalf("HasProvider(%q) = false, want true when another account of the same provider is registered", ref.Provider)
	}
	if reg.LookupByConversation(ref) != nil {
		t.Fatalf("LookupByConversation = non-nil, want nil for an unregistered (provider, account) pair")
	}
}

func TestAdapterRegistry_HasProviderAfterUnregister(t *testing.T) {
	reg := NewAdapterRegistry()
	ref := testConversationRef()
	key := AdapterKey{Provider: ref.Provider, AccountID: ref.AccountID}
	reg.Register(key, newStubAdapter("stub", ref))
	reg.Unregister(key)

	if reg.HasProvider(ref.Provider) {
		t.Fatalf("HasProvider(%q) after Unregister = true, want false", ref.Provider)
	}
}
