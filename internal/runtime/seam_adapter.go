package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// seamProvider implements the legacy [Provider] interface on top of the
// de-conflated seams (Runtime + Transport, and MetaStore when the runtime carries
// meta). It is the INVERSE of the per-provider seams.go wrappers: where those
// expose a *Provider AS the seams, this exposes the seams AS a Provider — so call
// sites that still depend on [Provider] can be served entirely by the seams. It
// is the vehicle for the cut-over: flip a provider's construction to route
// through the seams without touching the ~105 call sites.
//
// It is stateless-by-name: each call re-resolves the box via Runtime.Open and the
// live attachment via Transport.Open, matching how the stateless providers
// (exec/ssh/k8s) already behave.
//
// KNOWN GAPS — the seam contract does not yet cover these:
//   - RunLive: no seam exists, so this is a no-op — correct only for providers
//     whose RunLive is already a no-op (subprocess/exec/k8s/ssh/t3bridge).
//   - The optional extensions (ProcessTableScanner, SleepCapability,
//     InteractionProvider, ExecProvider, …) live outside the seams; a provider
//     needing them composes them alongside this adapter (see each provider's
//     cutover.go).
//
// ProcessAlive's per-call processNames ARE threaded through Attachment.Observe.
// A provider whose Observe ignores them (subprocess, which can only report
// IsRunning) does not reproduce the legacy "empty names => true" result, but
// that path is unreachable in production (both ProcessAlive callers gate on
// non-empty names).
type seamProvider struct {
	rt   Runtime
	tp   Transport
	meta MetaStore // rt asserted to MetaStore; nil when the runtime carries no meta
	// settle paces the stranded-submit retries (see recoverStrandedNudge). Nil
	// means time.Sleep; tests replace it so the retry logic runs without real
	// time passing.
	settle func(time.Duration)
}

// nudgeStrandRetryDelays paces the retries that recover a stranded submit, one
// entry per retry. The delays GROW rather than repeat because a confirming
// attachment only reports a strand after polling the pane through its own
// submit budget — by then the pane has already absorbed several submit
// keystrokes within a couple of seconds, and what it needs is time to settle,
// not another keystroke immediately.
//
// The budget is deliberately small. Delivery runs on the caller's critical path
// (`gc sling`, `gc mail notify`, the queue dispatcher), so this bounds the added
// latency of an already-failed nudge to a few seconds and then hands ownership
// back to the caller's queued-redelivery fallback.
var nudgeStrandRetryDelays = []time.Duration{time.Second, 2 * time.Second}

var (
	_ Provider = (*seamProvider)(nil)
	// The confirm pair is not optional for this adapter. Callers prefer
	// ConfirmingNudgeProvider over Nudge, so losing it here does not fail loudly
	// — it silently re-collapses every routed nudge to "delivered" and takes the
	// unsubmitted-draft fallbacks down with it (ga-287). Pin it at compile time.
	_ ConfirmingNudgeProvider = (*seamProvider)(nil)
)

// NewProviderFromSeams builds a legacy [Provider] backed by the given seams. If
// rt also implements [MetaStore], SetMeta/GetMeta/RemoveMeta route to it.
func NewProviderFromSeams(rt Runtime, tp Transport) Provider {
	meta, _ := rt.(MetaStore)
	return &seamProvider{rt: rt, tp: tp, meta: meta}
}

// Start provisions the box for name, then launches the agent IFF the transport
// reports a separable launch.
//
// For the welded carriers (tmux/ssh: the agent IS the new-session command; k8s:
// the pod entrypoint; welded exec packs: the `start` op launches), Provision
// alone launches the agent and SeparableLaunch is false, so Start does NOT call
// Launch — Transport.Launch is the SEPARATE relaunch-into-a-warm-box capability
// the reconciler uses for a launch-only change, and calling it here would launch
// the agent twice (see the un-weld design, B1/B3a).
//
// For an exec pack that declares proc.provision (B3b), Provision creates the box
// WITHOUT the agent (SeparableLaunch true), so Start must Provision THEN Launch.
func (s *seamProvider) Start(ctx context.Context, name string, cfg Config) error {
	place, err := s.rt.Provision(ctx, name, ProvisionRequest{Config: cfg})
	if err != nil {
		return err
	}
	if s.tp.Capabilities().SeparableLaunch {
		if _, err := s.tp.Launch(ctx, place, LaunchSpec{Config: cfg}); err != nil {
			// Provision created the box WITHOUT the agent (B3b); a failed Launch
			// would otherwise orphan it — the asymmetric opposite of the
			// unconditional Stop teardown. Tear it down best-effort before
			// surfacing the launch error so a separable-launch failure leaks no
			// box (SEAM-1/2/3 — no leaked boxes). If teardown ALSO fails the box
			// may still be running untracked, so surface BOTH errors instead of
			// hiding the cleanup failure behind the launch error.
			launchErr := fmt.Errorf("seam start %q: launch after provision: %w", name, err)
			if teardownErr := s.rt.Teardown(ctx, name); teardownErr != nil {
				return errors.Join(launchErr, fmt.Errorf("seam start %q: teardown after failed launch: %w", name, teardownErr))
			}
			return launchErr
		}
	}
	return nil
}

// attach re-resolves the live attachment for name, or (nil,false) when the box is
// not running.
func (s *seamProvider) attach(name string) (Attachment, bool) {
	ctx := context.Background()
	place, ok, err := s.rt.Open(ctx, name)
	if err != nil || !ok {
		return nil, false
	}
	att, ok, err := s.tp.Open(ctx, place, name)
	if err != nil || !ok {
		return nil, false
	}
	return att, true
}

func (s *seamProvider) Stop(name string) error {
	ctx := context.Background()
	// Best-effort detach the live attachment first (how-half). This is a no-op
	// for the carriers (their Attachment.Close is empty), but harmless and
	// correct for any stateful transport. We do NOT gate teardown on this: a box
	// that exists but is not running has no live attachment yet still must be
	// destroyed.
	if place, ok, err := s.rt.Open(ctx, name); err == nil && ok {
		if att, ok, _ := s.tp.Open(ctx, place, name); ok {
			_ = att.Close(ctx)
		}
	}
	// Teardown is UNCONDITIONAL (does not gate on liveness), so a non-Running
	// box is still torn down instead of leaking. (←Stop where-half; SEAM-1/2/3.)
	return s.rt.Teardown(ctx, name)
}

func (s *seamProvider) Interrupt(name string) error {
	if att, ok := s.attach(name); ok {
		return att.Interrupt(context.Background())
	}
	return nil
}

func (s *seamProvider) IsRunning(name string) bool {
	_, ok, _ := s.rt.Open(context.Background(), name)
	return ok
}

func (s *seamProvider) IsAttached(name string) bool {
	att, ok := s.attach(name)
	if !ok {
		return false
	}
	obs, err := att.Observe(context.Background(), nil)
	return err == nil && obs.Attached
}

func (s *seamProvider) Attach(name string) error {
	ctx := context.Background()
	place, ok, err := s.rt.Open(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, name)
	}
	return s.tp.Attach(ctx, place, name)
}

// ProcessAlive threads the per-call process names through Attachment.Observe.
func (s *seamProvider) ProcessAlive(name string, processNames []string) bool {
	att, ok := s.attach(name)
	if !ok {
		return false
	}
	obs, err := att.Observe(context.Background(), processNames)
	return err == nil && obs.ProcessAlive
}

func (s *seamProvider) Nudge(name string, content []ContentBlock) error {
	if att, ok := s.attach(name); ok {
		return att.Nudge(context.Background(), content)
	}
	return nil
}

// NudgeConfirm implements [ConfirmingNudgeProvider] by carrying the attachment's
// submit observation through the seam boundary.
//
// This is load-bearing rather than a convenience. The local tmux provider is
// served through this adapter in production (tmux.NewSeamBackedWithConfig), and
// a terminal runtime types the message and submits it as separate keystrokes —
// so it, alone in the stack, can observe that the submit never landed and the
// text is drafted in the agent's input box. Before this method existed the
// adapter could not express that, so the tmux wrapper fell through to its
// "cannot confirm, report delivered" branch and every routed nudge read as
// delivered. That made the wait-idle → queue fallback and the queued
// dispatcher's claim release unreachable, which is why an agent could sit on
// drafted wake-text for hours while every status surface read healthy (ga-287).
//
// An attachment with no confirmation signal reports true, per the
// [ConfirmingNudgeProvider] contract. A session with no live attachment reports
// false: plain Nudge keeps its best-effort nil ("the keystrokes were handed
// off"), but a message that reached no attachment plainly never ran, and
// claiming a delivery there recreates the same silent success.
//
// A submit the attachment reports as never landed is RETRIED here rather than
// handed straight back as a failure — see [seamProvider.recoverStrandedNudge].
func (s *seamProvider) NudgeConfirm(name string, content []ContentBlock) (bool, error) {
	att, ok := s.attach(name)
	if !ok {
		return false, nil
	}
	cp, ok := att.(ConfirmingNudgeAttachment)
	if !ok {
		return true, att.Nudge(context.Background(), content)
	}
	ctx := context.Background()
	submitted, err := cp.NudgeConfirm(ctx, content)
	if err != nil || submitted {
		return submitted, err
	}
	return s.recoverStrandedNudge(ctx, cp, content)
}

// NudgeNowConfirm implements [ConfirmingNudgeProvider] for immediate delivery.
// It routes to the attachment's own immediate path so an immediate nudge is not
// silently downgraded to the runtime's internal wait-idle step — that mode is
// how an operator recovers an already-stranded agent, so quietly re-routing it
// through the mechanism that stranded the agent defeats the remedy. See
// [seamProvider.NudgeConfirm] for the reporting contract.
func (s *seamProvider) NudgeNowConfirm(name string, content []ContentBlock) (bool, error) {
	att, ok := s.attach(name)
	if !ok {
		return false, nil
	}
	cp, ok := att.(ConfirmingNudgeAttachment)
	if !ok {
		return true, att.Nudge(context.Background(), content)
	}
	ctx := context.Background()
	submitted, err := cp.NudgeNowConfirm(ctx, content)
	if err != nil || submitted {
		return submitted, err
	}
	return s.recoverStrandedNudge(ctx, cp, content)
}

// recoverStrandedNudge re-attempts a nudge the attachment accepted but never
// observed submit — the text is drafted in the agent's input box and will sit
// there until something resubmits it.
//
// Detection alone was never the remedy (ga-287 -> ga-61tq -> ga-8tno). Reporting
// submitted=false only tells the CALLER to fall back, and the callers split two
// ways: those that cannot queue drop the message outright, and those that can
// re-run the identical delivery against the identical pane. Neither converges,
// so every recurrence needed a human or a patrol agent to re-nudge by hand.
//
// What that manual recovery actually supplied was a FRESH ATTEMPT AFTER THE PANE
// SETTLED, not a different delivery mode: `--delivery wait-idle` and
// `--delivery immediate` terminate in the same submit (internal/session/chat.go
// passes immediate=true on the wait-idle path), so "escalate to immediate" is
// not an escalation at all. Supplying the retry here makes a strand self-heal at
// the one layer every routed nudge passes through, which is also the only layer
// that can tell a strand from a delivery.
//
// Two properties make the retry safe:
//
//   - No double-submit. A confirming attachment reports false only after polling
//     the pane and never seeing the turn start, so the message provably has not
//     run. A landed submit returns before reaching here.
//   - Bounded. The budget is a few seconds (nudgeStrandRetryDelays); after that
//     ownership returns to the caller's queued-redelivery fallback rather than
//     holding delivery's critical path open.
//
// The retry re-types the message, so a recovered strand can show the text twice
// in one prompt — the same artifact a manual re-nudge produces, and strictly
// better than a message that never runs.
func (s *seamProvider) recoverStrandedNudge(ctx context.Context, cp ConfirmingNudgeAttachment, content []ContentBlock) (bool, error) {
	for _, delay := range nudgeStrandRetryDelays {
		s.settleBeforeRetry(delay)
		// Immediate, not the wait-idle confirm: the pane was just observed idle
		// through a full submit budget, so another wait would be pure latency.
		submitted, err := cp.NudgeNowConfirm(ctx, content)
		if err != nil || submitted {
			return submitted, err
		}
	}
	return false, nil
}

// settleBeforeRetry waits out one entry of the retry pacing, through the
// injected hook when a test supplied one.
func (s *seamProvider) settleBeforeRetry(d time.Duration) {
	if s.settle != nil {
		s.settle(d)
		return
	}
	time.Sleep(d)
}

func (s *seamProvider) SetMeta(name, key, value string) error {
	if s.meta == nil {
		return fmt.Errorf("runtime does not implement MetaStore")
	}
	return s.meta.SetMeta(name, key, value)
}

func (s *seamProvider) GetMeta(name, key string) (string, error) {
	if s.meta == nil {
		return "", nil
	}
	return s.meta.GetMeta(name, key)
}

func (s *seamProvider) RemoveMeta(name, key string) error {
	if s.meta == nil {
		return nil
	}
	return s.meta.RemoveMeta(name, key)
}

func (s *seamProvider) Peek(name string, lines int) (string, error) {
	if att, ok := s.attach(name); ok {
		return att.Peek(context.Background(), lines)
	}
	return "", nil
}

func (s *seamProvider) ListRunning(prefix string) ([]string, error) {
	return s.rt.List(context.Background(), prefix)
}

func (s *seamProvider) GetLastActivity(name string) (time.Time, error) {
	att, ok := s.attach(name)
	if !ok {
		return time.Time{}, nil
	}
	obs, err := att.Observe(context.Background(), nil)
	if err != nil {
		return time.Time{}, err
	}
	return obs.LastActivity, nil
}

func (s *seamProvider) ClearScrollback(name string) error {
	if att, ok := s.attach(name); ok {
		return att.ClearScrollback(context.Background())
	}
	return nil
}

func (s *seamProvider) CopyTo(name, src, relDst string) error {
	ctx := context.Background()
	place, ok, err := s.rt.Open(ctx, name)
	if err != nil || !ok {
		return nil // best-effort
	}
	return place.Stage(ctx, []CopyEntry{{Src: src, RelDst: relDst}})
}

func (s *seamProvider) SendKeys(name string, keys ...string) error {
	if att, ok := s.attach(name); ok {
		return att.SendKeys(context.Background(), keys...)
	}
	return nil
}

// RunLive is a no-op: there is no seam for live session_live re-apply (see GAPS).
func (s *seamProvider) RunLive(string, Config) error { return nil }

func (s *seamProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		CanReportActivity:   s.rt.Capabilities().ReportActivity,
		CanReportAttachment: s.tp.Capabilities().ReportAttachment,
	}
}
