package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBlockStartupHealthyOnHookReview(t *testing.T) {
	pane := "Hooks need review\n  2. Trust all and continue\n  3. Continue without trusting (hooks won't run)\n  Press enter to confirm or esc to go back\n"
	blocked, err := blockStartupHealthyOnHookReview("seat", nil, pane)
	if !blocked {
		t.Fatal("foreground dialog was not blocking")
	}
	if !errors.Is(err, runtime.ErrCodexHookReviewBlocked) {
		t.Fatalf("err = %v, want ErrCodexHookReviewBlocked", err)
	}

	blocked, exists := blockStartupHealthyOnHookReview("seat", runtime.ErrSessionExists, pane)
	if !blocked || !errors.Is(exists, runtime.ErrCodexHookReviewBlocked) {
		t.Fatalf("session-exists err = %v blocked=%v", exists, blocked)
	}

	other := errors.New("provider failed")
	blocked, got := blockStartupHealthyOnHookReview("seat", other, pane)
	if !blocked || !errors.Is(got, other) || errors.Is(got, runtime.ErrCodexHookReviewBlocked) {
		t.Fatalf("real failure was replaced: %v blocked=%v", got, blocked)
	}

	scrolled := pane + "❯ ready\n"
	if blocked, _ := blockStartupHealthyOnHookReview("seat", nil, scrolled); blocked {
		t.Fatal("scrollback dialog blocked a ready pane")
	}
}
