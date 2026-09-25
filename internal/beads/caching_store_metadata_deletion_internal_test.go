package beads

import (
	"context"
	"testing"
)

// TestCachingStoreReconcileDropsUnsetMetadataKey is the ga-cd1 probe: a key
// removed on the backing store (the rig row) must disappear from the cache
// the city controller serves (buildDesiredState), not survive as a stale
// work_dir after the next reconcile.
func TestCachingStoreReconcileDropsUnsetMetadataKey(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	created, err := backing.Create(Bead{
		Title: "rig work",
		Metadata: map[string]string{
			"work_dir": "/tmp/dead-worktree",
			"branch":   "polecat/ga-cd1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	primed, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after prime: %v", err)
	}
	if primed.Metadata["work_dir"] != "/tmp/dead-worktree" {
		t.Fatalf("primed work_dir = %q, want the rig value", primed.Metadata["work_dir"])
	}

	deleteBackingMetadataKey(t, backing, created.ID, "work_dir")
	cache.runReconciliation()

	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if _, ok := got.Metadata["work_dir"]; ok {
		t.Fatalf("city cache still has work_dir=%q after the rig store unset it", got.Metadata["work_dir"])
	}
	if got.Metadata["branch"] != "polecat/ga-cd1" {
		t.Fatalf("branch = %q, want the sibling key preserved", got.Metadata["branch"])
	}
}

// TestCachingStoreApplyEventDropsUnsetMetadataKey covers the hook path: a
// bead.updated snapshot whose metadata object no longer contains the key
// replaces the cached map, and a sibling key in that same object stays.
func TestCachingStoreApplyEventDropsUnsetMetadataKey(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	created, err := backing.Create(Bead{
		Title: "rig work",
		Type:  "task",
		Metadata: map[string]string{
			"work_dir": "/tmp/dead-worktree",
			"branch":   "polecat/ga-cd1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	deleteBackingMetadataKey(t, backing, created.ID, "work_dir")
	fresh, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get backing: %v", err)
	}
	payload, err := EncodeBeadEventPayload(fresh)
	if err != nil {
		t.Fatalf("EncodeBeadEventPayload: %v", err)
	}
	cache.ApplyEvent("bead.updated", payload)

	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after event: %v", err)
	}
	if _, ok := got.Metadata["work_dir"]; ok {
		t.Fatalf("city cache still has work_dir=%q after the update snapshot omitted it", got.Metadata["work_dir"])
	}
	if got.Metadata["branch"] != "polecat/ga-cd1" {
		t.Fatalf("branch = %q, want the sibling key preserved", got.Metadata["branch"])
	}
}

func deleteBackingMetadataKey(t *testing.T, m *MemStore, id, key string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.beads {
		if m.beads[i].ID != id {
			continue
		}
		if m.beads[i].Metadata == nil {
			t.Fatalf("bead %s has no metadata", id)
		}
		if _, ok := m.beads[i].Metadata[key]; !ok {
			t.Fatalf("bead %s metadata missing %s before unset", id, key)
		}
		delete(m.beads[i].Metadata, key)
		return
	}
	t.Fatalf("bead %s not found", id)
}
