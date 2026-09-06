package main

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"

	appservice "agent-overflow/internal/app"
)

// The pairing states as they appear ON THE WIRE. PairingStatusView documents
// these five spellings as its contract, and a test that fed a sixth would be
// testing nothing. Spelled once here so the cases below read as scripts.
const (
	wireStatePending   = "pending"
	wireStateRedeemed  = "redeemed"
	wireStateConfirmed = "confirmed"
	wireStateCanceled  = "canceled"
	wireStateExpired   = "expired"
)

// fakeEnrollment scripts one pairing exchange. Statuses are consumed in
// order and the last one sticks, so a case says "pending, pending, redeemed"
// and the poll loop settles there without a count.
type fakeEnrollment struct {
	enrolled    bool
	enrolledErr error
	invite      appservice.PairingInvite
	mintErr     error
	statuses    []appservice.PairingStatusView
	statusErr   error

	mints     int
	confirmed []string
	canceled  []string
}

func (f *fakeEnrollment) Enrolled() (bool, error) { return f.enrolled, f.enrolledErr }

func (f *fakeEnrollment) Mint() (appservice.PairingInvite, error) {
	f.mints++
	return f.invite, f.mintErr
}

func (f *fakeEnrollment) Status(string) (appservice.PairingStatusView, error) {
	if f.statusErr != nil {
		return appservice.PairingStatusView{}, f.statusErr
	}
	if len(f.statuses) == 0 {
		return appservice.PairingStatusView{State: wireStatePending}, nil
	}
	next := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return next, nil
}

func (f *fakeEnrollment) Confirm(linkID string) error {
	f.confirmed = append(f.confirmed, linkID)
	return nil
}

func (f *fakeEnrollment) Cancel(linkID string) error {
	f.canceled = append(f.canceled, linkID)
	return nil
}

// testConsole is a serveConsole over strings, with the terminal answer
// supplied rather than probed — which is the whole reason the probe is a
// field. No PTY is allocated anywhere in this file.
func testConsole(input string, interactive bool) (serveConsole, *strings.Builder) {
	out := &strings.Builder{}
	return serveConsole{
		in:          bufio.NewReader(strings.NewReader(input)),
		out:         out,
		interactive: func() bool { return interactive },
	}, out
}

func redeemedStatus() appservice.PairingStatusView {
	return appservice.PairingStatusView{
		State:              wireStateRedeemed,
		VerificationNumber: "471 208",
		DeviceLabel:        "Chrome on iPhone",
	}
}

func testInvite() appservice.PairingInvite {
	return appservice.PairingInvite{LinkID: "link-1", URL: "http://127.0.0.1:7777/#pair=abc"}
}

// A host that already has a paired device says nothing at all. This is every
// boot after the first, so a line here would be noise on every restart.
func TestServeEnrollmentIsSilentWhenADeviceIsPaired(t *testing.T) {
	surface := &fakeEnrollment{enrolled: true}
	console, out := testConsole("", true)

	runServeEnrollment(t.Context(), console, surface, time.Millisecond)

	if out.String() != "" {
		t.Fatalf("wrote %q to an enrolled host's console, want nothing", out.String())
	}
	if surface.mints != 0 {
		t.Fatalf("minted %d links on an enrolled host, want 0", surface.mints)
	}
}

// Under a service manager there is nobody to compare a number with, so the
// console must not mint a link into the journal. One line naming the remedy.
func TestServeEnrollmentUnderAServiceManagerNamesTheRemedy(t *testing.T) {
	surface := &fakeEnrollment{}
	console, out := testConsole("", false)

	runServeEnrollment(t.Context(), console, surface, time.Millisecond)

	if surface.mints != 0 {
		t.Fatalf("minted %d links with no console to confirm on, want 0", surface.mints)
	}
	text := out.String()
	if strings.Count(text, "\n") != 1 {
		t.Fatalf("wrote %d lines, want exactly one:\n%s", strings.Count(text, "\n"), text)
	}
	if !strings.Contains(text, "agent-overflow pair --lan") {
		t.Fatalf("the notice does not name the remedy:\n%s", text)
	}
}

// The whole exchange, on a console: mint, show the link, wait through a
// pending poll, show the number, take a yes, confirm.
func TestServeEnrollmentConfirmsOnYes(t *testing.T) {
	surface := &fakeEnrollment{
		invite: testInvite(),
		statuses: []appservice.PairingStatusView{
			{State: wireStatePending},
			{State: wireStatePending},
			redeemedStatus(),
		},
	}
	console, out := testConsole("y\n", true)

	runServeEnrollment(t.Context(), console, surface, time.Millisecond)

	text := out.String()
	if surface.mints != 1 {
		t.Fatalf("minted %d links, want 1", surface.mints)
	}
	if !strings.Contains(text, testInvite().URL) {
		t.Fatalf("the pairing URL was never printed:\n%s", text)
	}
	if !strings.Contains(text, "471 208") {
		t.Fatalf("the verification number was never printed:\n%s", text)
	}
	if !strings.Contains(text, "Chrome on iPhone") {
		t.Fatalf("the redeeming device was never named:\n%s", text)
	}
	if len(surface.confirmed) != 1 || surface.confirmed[0] != "link-1" {
		t.Fatalf("confirmed = %q, want one confirmation of link-1", surface.confirmed)
	}
	if len(surface.canceled) != 0 {
		t.Fatalf("canceled = %q on a confirmed pairing", surface.canceled)
	}
}

// Anything that is not an explicit yes cancels. The question's yes enrolls a
// device, so the default has to be the other one.
func TestServeEnrollmentCancelsOnAnythingButYes(t *testing.T) {
	for _, answer := range []string{"n\n", "no\n", "\n", "maybe\n", "  \n", "Y es\n"} {
		t.Run(strings.TrimSpace(answer)+"|", func(t *testing.T) {
			surface := &fakeEnrollment{invite: testInvite(), statuses: []appservice.PairingStatusView{redeemedStatus()}}
			console, out := testConsole(answer, true)

			runServeEnrollment(t.Context(), console, surface, time.Millisecond)

			if len(surface.confirmed) != 0 {
				t.Fatalf("confirmed %q on answer %q", surface.confirmed, answer)
			}
			if len(surface.canceled) != 1 {
				t.Fatalf("canceled = %q on answer %q, want one", surface.canceled, answer)
			}
			if !strings.Contains(out.String(), "Canceled") {
				t.Fatalf("the console does not say the pairing was refused:\n%s", out.String())
			}
		})
	}
}

// Case and surrounding space are the operator's, not the parser's.
func TestServeEnrollmentAcceptsYesInAnyCase(t *testing.T) {
	for _, answer := range []string{"y\n", "Y\n", "yes\n", "YES\n", " y \n", "y"} {
		t.Run(strings.TrimSpace(answer), func(t *testing.T) {
			surface := &fakeEnrollment{invite: testInvite(), statuses: []appservice.PairingStatusView{redeemedStatus()}}
			console, _ := testConsole(answer, true)

			runServeEnrollment(t.Context(), console, surface, time.Millisecond)

			if len(surface.confirmed) != 1 {
				t.Fatalf("confirmed = %q on answer %q, want one", surface.confirmed, answer)
			}
		})
	}
}

// A closed stdin is not an answer, and a redeemed link left waiting on one
// nobody can give is the state this cancels out of.
func TestServeEnrollmentCancelsWhenStdinIsClosed(t *testing.T) {
	surface := &fakeEnrollment{invite: testInvite(), statuses: []appservice.PairingStatusView{redeemedStatus()}}
	console, out := testConsole("", true)

	runServeEnrollment(t.Context(), console, surface, time.Millisecond)

	if len(surface.confirmed) != 0 {
		t.Fatalf("confirmed %q with no answer available", surface.confirmed)
	}
	if len(surface.canceled) != 1 {
		t.Fatalf("canceled = %q, want one", surface.canceled)
	}
	if !strings.Contains(out.String(), "Pairing did not finish") {
		t.Fatalf("the console does not report the failure:\n%s", out.String())
	}
}

// A link that ran out, or that somebody canceled elsewhere, ends the wait
// without confirming anything.
func TestServeEnrollmentReportsASettledLink(t *testing.T) {
	for _, state := range []string{wireStateExpired, wireStateCanceled} {
		t.Run(state, func(t *testing.T) {
			surface := &fakeEnrollment{invite: testInvite(), statuses: []appservice.PairingStatusView{{State: state}}}
			console, out := testConsole("y\n", true)

			runServeEnrollment(t.Context(), console, surface, time.Millisecond)

			if len(surface.confirmed) != 0 || len(surface.canceled) != 0 {
				t.Fatalf("acted on a %s link: confirmed=%q canceled=%q", state, surface.confirmed, surface.canceled)
			}
			if !strings.Contains(out.String(), state) {
				t.Fatalf("the console does not say the link is %s:\n%s", state, out.String())
			}
		})
	}
}

// Confirmed from another surface while this waited is a complete outcome,
// not a race to report as a failure — and confirming a second time would be
// an error the operator did not cause.
func TestServeEnrollmentAcceptsAConfirmationFromElsewhere(t *testing.T) {
	surface := &fakeEnrollment{invite: testInvite(), statuses: []appservice.PairingStatusView{{State: wireStateConfirmed}}}
	console, out := testConsole("", true)

	runServeEnrollment(t.Context(), console, surface, time.Millisecond)

	if len(surface.confirmed) != 0 || len(surface.canceled) != 0 {
		t.Fatalf("acted on an already-confirmed link: confirmed=%q canceled=%q", surface.confirmed, surface.canceled)
	}
	if !strings.Contains(out.String(), "confirmed somewhere else") {
		t.Fatalf("the console does not report the outcome:\n%s", out.String())
	}
}

// Shutdown during the wait ends the goroutine. Without this, Ctrl-C while a
// link was pending would leave the poll running until the process died.
func TestServeEnrollmentStopsWhenTheBootContextEnds(t *testing.T) {
	surface := &fakeEnrollment{invite: testInvite(), statuses: []appservice.PairingStatusView{{State: wireStatePending}}}
	console, _ := testConsole("", true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan struct{})
	go func() {
		runServeEnrollment(ctx, console, surface, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enrollment did not stop when the boot context was cancelled")
	}
	if len(surface.confirmed) != 0 || len(surface.canceled) != 0 {
		t.Fatalf("acted on a link during shutdown: confirmed=%q canceled=%q", surface.confirmed, surface.canceled)
	}
}

// A backend that cannot answer "which devices are paired" must still serve.
// It reports and steps aside rather than minting a link it cannot reason
// about.
func TestServeEnrollmentReportsAFailedEnrolledCheck(t *testing.T) {
	surface := &fakeEnrollment{enrolledErr: context.DeadlineExceeded}
	console, out := testConsole("", true)

	runServeEnrollment(t.Context(), console, surface, time.Millisecond)

	if surface.mints != 0 {
		t.Fatalf("minted %d links after a failed check, want 0", surface.mints)
	}
	if !strings.Contains(out.String(), "Could not check") {
		t.Fatalf("the console does not report the failure:\n%s", out.String())
	}
}

// The class and access level this mode enrolls are the ones identity
// declares, not strings this package invented. A vocabulary change should
// break the build or this test, never produce a silently unpaired host.
func TestServeEnrollmentUsesDeclaredIdentityVocabulary(t *testing.T) {
	if serveEnrollmentDeviceClass != "browser" {
		t.Fatalf("serve enrolls device class %q, want browser", serveEnrollmentDeviceClass)
	}
	if serveEnrollmentAccess != "full" {
		t.Fatalf("serve enrolls with access %q, want full", serveEnrollmentAccess)
	}
}
