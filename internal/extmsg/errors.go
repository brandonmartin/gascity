package extmsg

import (
	"errors"
	"fmt"
)

// Sentinel errors for extmsg operations.
var (
	ErrUnauthorized         = errors.New("extmsg unauthorized")
	ErrInvalidCaller        = errors.New("extmsg invalid caller")
	ErrInvalidInput         = errors.New("extmsg invalid input")
	ErrInvalidConversation  = errors.New("extmsg invalid conversation")
	ErrInvalidHandle        = errors.New("extmsg invalid handle")
	ErrBindingConflict      = errors.New("extmsg binding conflict")
	ErrBindingMismatch      = errors.New("extmsg binding mismatch")
	ErrInvariantViolation   = errors.New("extmsg invariant violation")
	ErrGroupNotFound        = errors.New("extmsg group not found")
	ErrGroupRouteNotFound   = errors.New("extmsg group route not found")
	ErrMembershipNotFound   = errors.New("extmsg membership not found")
	ErrHydrationPending     = errors.New("extmsg hydration pending")
	ErrHydrationFailed      = errors.New("extmsg hydration failed")
	ErrTranscriptSyncFailed = errors.New("extmsg transcript sync failed")

	// ErrProviderNotRegistered reports that a conversation's provider has no
	// transport adapter registered at all, so no conversation on it can be
	// delivered or meaningfully bound.
	ErrProviderNotRegistered = errors.New("extmsg provider not registered")
)

func wrapTranscriptSyncError(action string, err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(ErrTranscriptSyncFailed, fmt.Errorf("%s: %w", action, err))
}
