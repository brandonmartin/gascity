package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// poolInstanceWorkDirCity builds a two-slot namepool whose work_dir template
// expands {{.AgentBase}}, so the template identity ("worker") and each pool
// instance identity ("furiosa", "nux") resolve to distinct directories.
func poolInstanceWorkDirCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			WorkDir:           ".gc/worktrees/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
			NamepoolNames:     []string{"furiosa", "nux"},
		}},
	}
}

// TestRealizePoolDesiredSessionsRebindKeepsPerInstanceWorkDir pins the fix for
// ga-po0o: re-pointing an already-provisioned pool session at a NEW work bead
// must keep that session's per-instance working directory.
//
// realizePoolDesiredSessions used to bind the trigger bead before deriving the
// slot's instance identity, so the rebind resolved work_dir against the pool
// TEMPLATE ("worker") instead of the slot instance ("nux"). Every reused pool
// session then collapsed onto one shared directory — the symptom reported
// against five concurrent polecats sharing a single session cwd.
func TestRealizePoolDesiredSessionsRebindKeepsPerInstanceWorkDir(t *testing.T) {
	cityPath := t.TempDir()
	instanceWorkDir := filepath.Join(cityPath, ".gc", "worktrees", "nux")
	templateWorkDir := filepath.Join(cityPath, ".gc", "worktrees", "worker")

	store := beads.NewMemStore()
	reusable, err := store.Create(beads.Bead{
		Title:  "worker slot 2",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                        "worker",
			"agent_name":                      "nux",
			"alias":                           "nux",
			"session_name":                    "worker-nux",
			"state":                           string(sessionpkg.StateAwake),
			"wake_mode":                       "fresh",
			"pool_slot":                       "2",
			poolManagedMetadataKey:            boolMetadata(true),
			beadmeta.TriggerBeadIDMetadataKey: "wb-old",
			beadmeta.WorkDirMetadataKey:       instanceWorkDir,
			beadmeta.LegacyWorkDirMetadataKey: instanceWorkDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := poolInstanceWorkDirCity()
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, reusable))
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = snapshot

	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "worker",
		Requests: []SessionRequest{{
			Template:      "worker",
			Tier:          "wake-known-identity",
			SessionBeadID: reusable.ID,
			WorkBeadID:    "wb-new",
		}},
	}, map[string]TemplateParams{}, &stderr)

	stored, err := store.Get(reusable.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := stored.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "wb-new" {
		t.Fatalf("trigger bead metadata = %q, want wb-new (rebind did not happen)", got)
	}
	if got := stored.Metadata[beadmeta.WorkDirMetadataKey]; got != instanceWorkDir {
		t.Fatalf("gc.work_dir = %q, want per-instance %q (template base is %q)", got, instanceWorkDir, templateWorkDir)
	}
	if got := stored.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != instanceWorkDir {
		t.Fatalf("work_dir = %q, want per-instance %q (template base is %q)", got, instanceWorkDir, templateWorkDir)
	}
}

// TestRealizePoolDesiredSessionsRebindGivesEachSlotItsOwnWorkDir is the
// fleet-level statement of the same invariant: two pool slots rebound to new
// work in the same tick must not converge on one shared session directory.
func TestRealizePoolDesiredSessionsRebindGivesEachSlotItsOwnWorkDir(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	snapshot := &sessionBeadSnapshot{}

	slots := []struct {
		slot     string
		instance string
	}{
		{slot: "1", instance: "furiosa"},
		{slot: "2", instance: "nux"},
	}
	ids := make([]string, 0, len(slots))
	for _, s := range slots {
		workDir := filepath.Join(cityPath, ".gc", "worktrees", s.instance)
		bead, err := store.Create(beads.Bead{
			Title:  "worker " + s.instance,
			Type:   sessionBeadType,
			Status: "open",
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"template":                        "worker",
				"agent_name":                      s.instance,
				"alias":                           s.instance,
				"session_name":                    "worker-" + s.instance,
				"state":                           string(sessionpkg.StateAwake),
				"wake_mode":                       "fresh",
				"pool_slot":                       s.slot,
				poolManagedMetadataKey:            boolMetadata(true),
				beadmeta.TriggerBeadIDMetadataKey: "wb-old-" + s.instance,
				beadmeta.WorkDirMetadataKey:       workDir,
				beadmeta.LegacyWorkDirMetadataKey: workDir,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, bead.ID)
		snapshot.addInfo(sessiontest.SeedBead(t, bead))
	}

	cfg := poolInstanceWorkDirCity()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = snapshot

	requests := make([]SessionRequest, 0, len(ids))
	for i, id := range ids {
		requests = append(requests, SessionRequest{
			Template:      "worker",
			Tier:          "wake-known-identity",
			SessionBeadID: id,
			WorkBeadID:    "wb-new-" + slots[i].instance,
		})
	}
	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "worker",
		Requests: requests,
	}, map[string]TemplateParams{}, &stderr)

	seen := make(map[string]string, len(ids))
	for _, id := range ids {
		stored, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(session %s): %v", id, err)
		}
		workDir := stored.Metadata[beadmeta.WorkDirMetadataKey]
		if prior, dup := seen[workDir]; dup {
			t.Fatalf("sessions %s and %s share work_dir %q; each pool slot needs its own directory", prior, id, workDir)
		}
		seen[workDir] = id
	}
}
