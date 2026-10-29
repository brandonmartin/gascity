package beads_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// wispTierRunner is a bd runner over two tiers: rows in issues answer bd show,
// rows in wisps answer the ephemeral-by-id query. It records every command so a
// test can count the round trips a Get spent.
type wispTierRunner struct {
	mu        sync.Mutex
	issues    map[string]string
	wisps     map[string]string
	wispErr   error
	calls     []string
	wispCalls int
}

func (r *wispTierRunner) run(_, _ string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cmd := strings.Join(args, " ")
	r.calls = append(r.calls, cmd)
	switch {
	case len(args) >= 3 && args[0] == "show":
		id := args[2]
		if row, ok := r.issues[id]; ok {
			return []byte("[" + row + "]"), nil
		}
		return nil, fmt.Errorf("issue %s not found", id)
	case len(args) >= 3 && args[0] == "query" && strings.HasPrefix(args[2], "ephemeral=true AND id="):
		r.wispCalls++
		if r.wispErr != nil {
			return nil, r.wispErr
		}
		id := strings.TrimPrefix(args[2], "ephemeral=true AND id=")
		if row, ok := r.wisps[id]; ok {
			return []byte("[" + row + "]"), nil
		}
		return []byte(`[]`), nil
	}
	return nil, fmt.Errorf("unexpected bd %s", cmd)
}

func (r *wispTierRunner) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
	r.wispCalls = 0
}

func (r *wispTierRunner) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func wispRow(id string) string {
	return fmt.Sprintf(`{"id":%q,"title":"seat","status":"open","issue_type":"session","ephemeral":true}`, id)
}

func issueRow(id, title string) string {
	return fmt.Sprintf(`{"id":%q,"title":%q,"status":"open","issue_type":"session"}`, id, title)
}

func TestBdStoreGetAsksWispTierFirstForAKnownWisp(t *testing.T) {
	runner := &wispTierRunner{wisps: map[string]string{"gc-wisp-s1": wispRow("gc-wisp-s1")}}
	s := beads.NewBdStore("/city", runner.run)

	if _, err := s.Get("gc-wisp-s1"); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if got := runner.commands(); len(got) != 2 || !strings.HasPrefix(got[0], "show ") {
		t.Fatalf("first Get commands = %q, want bd show then the wisp-tier query", got)
	}

	runner.reset()
	b, err := s.Get("gc-wisp-s1")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if b.ID != "gc-wisp-s1" || !b.Ephemeral || b.Title != "seat" {
		t.Fatalf("second Get = %+v, want the wisp row", b)
	}
	if got := runner.commands(); len(got) != 1 || !strings.HasPrefix(got[0], "query ") {
		t.Fatalf("second Get commands = %q, want only the wisp-tier query", got)
	}
}

func TestBdStoreGetFindsAKnownWispThatLeftTheWispTier(t *testing.T) {
	runner := &wispTierRunner{wisps: map[string]string{"gc-wisp-s2": wispRow("gc-wisp-s2")}}
	s := beads.NewBdStore("/city", runner.run)
	if _, err := s.Get("gc-wisp-s2"); err != nil {
		t.Fatalf("first Get: %v", err)
	}

	// The row now lives in the issues table only.
	runner.mu.Lock()
	delete(runner.wisps, "gc-wisp-s2")
	runner.issues = map[string]string{"gc-wisp-s2": issueRow("gc-wisp-s2", "moved")}
	runner.mu.Unlock()
	runner.reset()

	b, err := s.Get("gc-wisp-s2")
	if err != nil {
		t.Fatalf("Get after move: %v", err)
	}
	if b.Title != "moved" || b.Ephemeral {
		t.Fatalf("Get after move = %+v, want the issues row", b)
	}
	// One wisp-tier query (the hint), then bd show. A clean wisp-tier miss is
	// not asked again.
	if got := runner.commands(); len(got) != 2 || !strings.HasPrefix(got[0], "query ") || !strings.HasPrefix(got[1], "show ") {
		t.Fatalf("Get after move commands = %q, want the wisp-tier query then bd show", got)
	}

	// The hint is gone: the next Get asks bd show first.
	runner.reset()
	if _, err := s.Get("gc-wisp-s2"); err != nil {
		t.Fatalf("third Get: %v", err)
	}
	if got := runner.commands(); len(got) != 1 || !strings.HasPrefix(got[0], "show ") {
		t.Fatalf("third Get commands = %q, want only bd show", got)
	}
}

func TestBdStoreGetKnownWispGoneFromBothTiersIsNotFound(t *testing.T) {
	runner := &wispTierRunner{wisps: map[string]string{"gc-wisp-s3": wispRow("gc-wisp-s3")}}
	s := beads.NewBdStore("/city", runner.run)
	if _, err := s.Get("gc-wisp-s3"); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	runner.mu.Lock()
	delete(runner.wisps, "gc-wisp-s3")
	runner.mu.Unlock()
	runner.reset()

	_, err := s.Get("gc-wisp-s3")
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get of a deleted wisp: err = %v, want ErrNotFound", err)
	}
	if runner.wispCalls != 1 {
		t.Fatalf("wisp-tier queries = %d (%q), want 1", runner.wispCalls, runner.commands())
	}
}

func TestBdStoreGetKnownWispTierFailureFallsBackToTheFullRead(t *testing.T) {
	runner := &wispTierRunner{wisps: map[string]string{"gc-wisp-s4": wispRow("gc-wisp-s4")}}
	s := beads.NewBdStore("/city", runner.run)
	if _, err := s.Get("gc-wisp-s4"); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	runner.mu.Lock()
	runner.wispErr = fmt.Errorf("exit status 1: dial tcp 127.0.0.1:3307: connection refused")
	runner.mu.Unlock()
	runner.reset()

	_, err := s.Get("gc-wisp-s4")
	if err == nil || errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get with a failing wisp tier: err = %v, want the wisp-tier failure, not ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want it to carry the wisp-tier failure", err)
	}
	// The failed hinted query proves nothing, so the full read runs: bd show,
	// then the wisp tier again.
	got := runner.commands()
	if len(got) < 3 || !strings.HasPrefix(got[0], "query ") || !strings.HasPrefix(got[1], "show ") {
		t.Fatalf("commands = %q, want the hinted query, bd show, then the wisp tier again", got)
	}
}

func TestBdStoreGetAnsweredByBdShowLeavesNoWispHint(t *testing.T) {
	runner := &wispTierRunner{issues: map[string]string{"gc-wisp-s5": issueRow("gc-wisp-s5", "issue")}}
	s := beads.NewBdStore("/city", runner.run)
	for i := 0; i < 2; i++ {
		runner.reset()
		if _, err := s.Get("gc-wisp-s5"); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		if got := runner.commands(); len(got) != 1 || !strings.HasPrefix(got[0], "show ") {
			t.Fatalf("Get %d commands = %q, want only bd show", i, got)
		}
	}
}
