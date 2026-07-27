package runtime

import (
	"context"
	"errors"
	"testing"
)

// These tests pin the seam adapter's half of the unsubmitted-nudge contract
// (ga-287). A terminal runtime types the message and submits it as SEPARATE
// keystrokes, so it can observe that the submit never landed and the text is
// sitting drafted in the agent's input box. That observation is worthless unless
// every layer between the runtime and the caller carries it.
//
// seamProvider is such a layer, and it is not an incidental one: the local tmux
// provider is served through it in production (tmux.NewSeamBackedWithConfig, the
// constructor cmd/gc's runtime registry uses). A seam adapter that cannot express
// "typed but never submitted" collapses every routed nudge to delivered, which
// makes the wait-idle -> queue fallback and the queued dispatcher's claim release
// unreachable no matter how honest the runtime underneath is.

// confirmingSeamAttachment is a seam Attachment whose runtime CAN observe
// whether an injected nudge actually submitted — the terminal-runtime shape.
type confirmingSeamAttachment struct {
	fakeSeamAttachment
	submitted   bool  // what the runtime observed about the submit
	confirmErr  error // when non-nil, NudgeConfirm/NudgeNowConfirm fail with this
	nudges      int   // plain Nudge calls (the wait-idle path)
	confirms    int   // NudgeConfirm calls
	nowConfirms int   // NudgeNowConfirm calls
}

func (a *confirmingSeamAttachment) Nudge(context.Context, []ContentBlock) error {
	a.nudges++
	return nil
}

func (a *confirmingSeamAttachment) NudgeConfirm(context.Context, []ContentBlock) (bool, error) {
	a.confirms++
	return a.submitted, a.confirmErr
}

func (a *confirmingSeamAttachment) NudgeNowConfirm(context.Context, []ContentBlock) (bool, error) {
	a.nowConfirms++
	return a.submitted, a.confirmErr
}

// confirmingSeamTransport hands out one stable confirming attachment so a test
// can inspect the call counts after the provider re-resolves it per call.
type confirmingSeamTransport struct {
	fakeSeamTransport
	att *confirmingSeamAttachment
}

func (t *confirmingSeamTransport) Open(context.Context, Place, string) (Attachment, bool, error) {
	if t.att == nil {
		return nil, false, nil
	}
	return t.att, true, nil
}

// newConfirmingSeamProvider wires a running box to att.
func newConfirmingSeamProvider(att *confirmingSeamAttachment) Provider {
	rt := &fakeSeamRuntime{openOK: true}
	tp := &confirmingSeamTransport{att: att}
	return NewProviderFromSeams(rt, tp)
}

// confirmingProvider asserts sp exposes the confirm pair at all. The failure this
// guards is not a wrong bool — it is the interface being absent, so callers that
// prefer ConfirmingNudgeProvider silently fall through to the "cannot observe,
// report true" branch and the runtime's real observation is discarded.
func confirmingProvider(t *testing.T, sp Provider) ConfirmingNudgeProvider {
	t.Helper()
	cp, ok := sp.(ConfirmingNudgeProvider)
	if !ok {
		t.Fatalf("seam-backed Provider does not implement ConfirmingNudgeProvider; every routed nudge reports delivered even when the submit never landed (ga-287)")
	}
	return cp
}

// TestSeamProviderNudgeConfirmReportsUnsubmittedDraft is the core regression
// guard: the runtime observed that the submit never landed, so the seam adapter
// must say so instead of reporting a delivery.
func TestSeamProviderNudgeConfirmReportsUnsubmittedDraft(t *testing.T) {
	att := &confirmingSeamAttachment{submitted: false}
	cp := confirmingProvider(t, newConfirmingSeamProvider(att))

	submitted, err := cp.NudgeConfirm("sess-1", TextContent("keep patrolling"))
	if err != nil {
		t.Fatalf("NudgeConfirm: %v", err)
	}
	if submitted {
		t.Fatal("NudgeConfirm reported submitted=true for a draft the runtime never saw submit; the wait-idle -> queue fallback stays unreachable and the agent sits on the drafted text")
	}
	if att.confirms != 1 {
		t.Fatalf("attachment NudgeConfirm calls = %d, want 1 (the routed nudge must reach the confirming path)", att.confirms)
	}
	if att.nudges != 0 {
		t.Fatalf("attachment plain Nudge calls = %d, want 0 (the confirming path must not double-deliver)", att.nudges)
	}
}

// TestSeamProviderNudgeConfirmReportsSubmitted pins the happy path: a landed
// submit still reads as delivered, so honest reporting does not downgrade
// working delivery into needless queueing.
func TestSeamProviderNudgeConfirmReportsSubmitted(t *testing.T) {
	att := &confirmingSeamAttachment{submitted: true}
	cp := confirmingProvider(t, newConfirmingSeamProvider(att))

	submitted, err := cp.NudgeConfirm("sess-1", TextContent("keep patrolling"))
	if err != nil {
		t.Fatalf("NudgeConfirm: %v", err)
	}
	if !submitted {
		t.Fatal("NudgeConfirm reported submitted=false for a submit the runtime confirmed landed")
	}
}

// TestSeamProviderNudgeNowConfirmUsesImmediatePath pins the second half of the
// pair. NudgeNowConfirm must reach the attachment's immediate confirm, NOT the
// plain Nudge that carries the runtime's internal wait-idle step: routing an
// immediate nudge through the wait-idle path silently downgrades the one
// delivery mode operators use to recover a stranded agent.
func TestSeamProviderNudgeNowConfirmUsesImmediatePath(t *testing.T) {
	att := &confirmingSeamAttachment{submitted: false}
	cp := confirmingProvider(t, newConfirmingSeamProvider(att))

	submitted, err := cp.NudgeNowConfirm("sess-1", TextContent("keep patrolling"))
	if err != nil {
		t.Fatalf("NudgeNowConfirm: %v", err)
	}
	if submitted {
		t.Fatal("NudgeNowConfirm reported submitted=true for a draft the runtime never saw submit")
	}
	if att.nowConfirms != 1 {
		t.Fatalf("attachment NudgeNowConfirm calls = %d, want 1 (immediate delivery must not be routed through the wait-idle Nudge)", att.nowConfirms)
	}
	if att.nudges != 0 || att.confirms != 0 {
		t.Fatalf("attachment took the wait-idle path: Nudge=%d NudgeConfirm=%d, want 0/0", att.nudges, att.confirms)
	}
}

// TestSeamProviderNudgeConfirmSurfacesAttachmentError pins error propagation:
// a runtime-level failure must not be laundered into a clean "unsubmitted"
// verdict, because the two demand different recovery.
func TestSeamProviderNudgeConfirmSurfacesAttachmentError(t *testing.T) {
	sentinel := errors.New("send-keys failed")
	att := &confirmingSeamAttachment{submitted: false, confirmErr: sentinel}
	cp := confirmingProvider(t, newConfirmingSeamProvider(att))

	if _, err := cp.NudgeConfirm("sess-1", TextContent("keep patrolling")); !errors.Is(err, sentinel) {
		t.Fatalf("NudgeConfirm error = %v, want %v", err, sentinel)
	}
	if _, err := cp.NudgeNowConfirm("sess-1", TextContent("keep patrolling")); !errors.Is(err, sentinel) {
		t.Fatalf("NudgeNowConfirm error = %v, want %v", err, sentinel)
	}
}

// TestSeamProviderNudgeConfirmReportsTrueWithoutRuntimeObservation pins the
// documented no-signal contract from ConfirmingNudgeProvider: an attachment that
// cannot observe the agent reports TRUE. Absence of a confirmation signal is not
// evidence of a failed submit, and reporting false here would downgrade every
// non-terminal runtime's delivery to queued.
func TestSeamProviderNudgeConfirmReportsTrueWithoutRuntimeObservation(t *testing.T) {
	rt := &fakeSeamRuntime{openOK: true}
	tp := &fakeSeamTransport{openOK: true} // plain attachment: no confirm pair
	cp := confirmingProvider(t, NewProviderFromSeams(rt, tp))

	for _, tc := range []struct {
		name string
		call func() (bool, error)
	}{
		{"NudgeConfirm", func() (bool, error) { return cp.NudgeConfirm("sess-1", TextContent("hi")) }},
		{"NudgeNowConfirm", func() (bool, error) { return cp.NudgeNowConfirm("sess-1", TextContent("hi")) }},
	} {
		submitted, err := tc.call()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !submitted {
			t.Fatalf("%s reported submitted=false for a runtime with no confirmation signal; that downgrades working best-effort delivery", tc.name)
		}
	}
}

// TestSeamProviderNudgeConfirmReportsUnsubmittedForVanishedSession pins the
// no-attachment case. Plain Nudge keeps its best-effort nil ("the keystrokes
// were handed off"), but a session that is gone never ran the message, so the
// confirm pair must not claim a delivery — that is the same silent success the
// bead is about.
func TestSeamProviderNudgeConfirmReportsUnsubmittedForVanishedSession(t *testing.T) {
	rt := &fakeSeamRuntime{openOK: true}
	tp := &fakeSeamTransport{openOK: false} // box resolves, but nothing to attach to
	cp := confirmingProvider(t, NewProviderFromSeams(rt, tp))

	for _, tc := range []struct {
		name string
		call func() (bool, error)
	}{
		{"NudgeConfirm", func() (bool, error) { return cp.NudgeConfirm("gone", TextContent("hi")) }},
		{"NudgeNowConfirm", func() (bool, error) { return cp.NudgeNowConfirm("gone", TextContent("hi")) }},
	} {
		submitted, err := tc.call()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if submitted {
			t.Fatalf("%s reported submitted=true for a session with no live attachment", tc.name)
		}
	}
}
