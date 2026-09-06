package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/identity"
)

// First-device enrollment from the serve console
// (docs/specs/remote-access.md §4; walkthrough in
// docs/architecture/serve-mode.md).
//
// The problem this solves exists only on a headless host. Pairing needs an
// OWNER SURFACE — something already trusted, that shows the operator the
// verification number the new device is displaying and takes their yes or
// no. Every other boot mode has one: a window. A serve host has a
// terminal, and nothing else.
//
// So the terminal becomes it, on exactly the same terms. Nothing here
// decides anything about identity: the flow calls MintDevicePairing,
// DevicePairingStatus and ConfirmDevicePairing — the same four methods the
// settings screen calls, in the same order — and identity applies its own
// rules to each. The single-use link, the proof of possession, the
// five-minute window, the confirmation window, the verification number
// derived from the device's own key: all of it is unchanged and none of it
// is reachable from here.
//
// The one thing this DOES assert is that the caller is host-present, and
// it does not assert it by claiming anything. An in-process call carries
// no session context, and app_authz.go admits that caller by construction
// (its file header names the class). Holding a TTY on the machine is the
// same standing-at-it that the step-up requirement already recognises.
//
// Everything the console touches is injected — reader, writer, and the
// "is this a terminal" probe — so the whole exchange is driven by a test
// with no PTY anywhere.

const (
	// serveEnrollmentDeviceClass and serveEnrollmentAccess are argued at
	// appEnrollment.Mint. Spelled through identity's own constants so the
	// class this mode enrolls cannot drift from the declared vocabulary.
	serveEnrollmentDeviceClass = string(identity.DeviceBrowser)
	serveEnrollmentAccess      = string(identity.PairingAccessFull)
)

// serveEnrollment is the slice of the owner surface the console drives.
// An interface rather than a *App so a test can supply a scripted pairing
// exchange; appEnrollment in main_serve.go is the production one and does
// nothing but forward.
type serveEnrollment interface {
	Enrolled() (bool, error)
	Mint() (appservice.PairingInvite, error)
	Status(linkID string) (appservice.PairingStatusView, error)
	Confirm(linkID string) error
	Cancel(linkID string) error
}

// serveConsole is the terminal, injected.
type serveConsole struct {
	in  *bufio.Reader
	out io.Writer
	// interactive answers whether a person is on the other end of in. It
	// is a func rather than a bool so production can ask the OS and a test
	// can answer either way without one.
	interactive func() bool
}

func defaultServeConsole() serveConsole {
	return serveConsole{
		in:          bufio.NewReader(os.Stdin),
		out:         os.Stdout,
		interactive: stdinIsTerminal,
	}
}

// stdinIsTerminal reports whether stdin is a character device.
//
// A character device is the property that matters, and it is what
// separates the two callers this mode has: a person at a terminal gets one,
// while systemd and launchd hand a service /dev/null or a socket. Deciding
// from the mode bits rather than an ioctl keeps this stdlib-only and
// correct on every platform the mode runs on — a pipe, a regular file and
// a redirected journal are all non-interactive, which is the right answer
// for each.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		// Something is wrong with fd 0. Treat that as "nobody is there",
		// which fails toward printing a remedy instead of blocking a
		// service on a read that will never return.
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// runServeEnrollment offers the console as the owner surface when this
// backend has no device that could reach it, and returns immediately when
// it has one.
//
// Errors are printed, never fatal. A backend that refused to serve because
// it could not offer a pairing link would be strictly worse than one an
// operator has to enroll a different way — and the different way exists:
// the Settings → Access screen, from any device already paired.
func runServeEnrollment(ctx context.Context, console serveConsole, surface serveEnrollment, poll time.Duration) {
	enrolled, err := surface.Enrolled()
	if err != nil {
		fmt.Fprintf(console.out, "Could not check which devices are paired with this backend: %v\n", err)
		return
	}
	if enrolled {
		return
	}
	if !console.interactive() {
		// One line, naming the remedy. A service manager's operator reads
		// this in the journal after wondering why they cannot sign in, so
		// it has to say what to do rather than what happened.
		fmt.Fprintln(console.out,
			"No device is paired with this backend, and nothing here can confirm a new one: "+
				"run `agent-overflow pair --lan` from a terminal to pair a device while the service keeps running.")
		return
	}
	if err := enrollFirstDevice(ctx, console, surface, poll); err != nil {
		fmt.Fprintf(console.out, "Pairing did not finish: %v\n", err)
	}
}

// enrollFirstDevice runs the exchange: mint, show, wait, compare, confirm.
func enrollFirstDevice(ctx context.Context, console serveConsole, surface serveEnrollment, poll time.Duration) error {
	invite, err := surface.Mint()
	if err != nil {
		return err
	}
	fmt.Fprintln(console.out)
	fmt.Fprintln(console.out, "No device is paired with this backend yet.")
	fmt.Fprintln(console.out, "Open this link on the device you want to use:")
	fmt.Fprintln(console.out)
	fmt.Fprintf(console.out, "  %s\n", invite.URL)
	fmt.Fprintln(console.out)
	fmt.Fprintln(console.out, "It works once, and it stops working in a few minutes.")

	status, err := awaitRedemption(ctx, surface, invite.LinkID, poll)
	if err != nil {
		return err
	}
	if status.Confirmed() {
		// Somebody confirmed from another surface while this waited. That
		// is a complete outcome, not a race to report as a failure.
		fmt.Fprintln(console.out, "This link was confirmed somewhere else. That device is paired.")
		return nil
	}
	if !status.Redeemed() {
		return fmt.Errorf("the link is %s", status.State)
	}

	fmt.Fprintln(console.out)
	if status.DeviceLabel != "" {
		fmt.Fprintf(console.out, "%s opened the link and is showing a number.\n", status.DeviceLabel)
	} else {
		fmt.Fprintln(console.out, "A device opened the link and is showing a number.")
	}
	fmt.Fprintf(console.out, "This backend's number is: %s\n", status.VerificationNumber)
	fmt.Fprintln(console.out)
	fmt.Fprintln(console.out, "Confirm only if the device shows the same number.")

	confirm, err := askYesNo(console, "Do the numbers match? [y/N] ")
	if err != nil {
		// A closed or unreadable stdin is not a yes. Cancel, so a redeemed
		// link never sits waiting on an answer nobody can give.
		if cancelErr := surface.Cancel(invite.LinkID); cancelErr != nil {
			return fmt.Errorf("read the answer: %w (canceling the link also failed: %v)", err, cancelErr)
		}
		return fmt.Errorf("read the answer: %w", err)
	}
	if !confirm {
		if err := surface.Cancel(invite.LinkID); err != nil {
			return fmt.Errorf("cancel the link: %w", err)
		}
		fmt.Fprintln(console.out, "Canceled. That device holds nothing, and the link is spent.")
		return nil
	}
	if err := surface.Confirm(invite.LinkID); err != nil {
		return fmt.Errorf("confirm the link: %w", err)
	}
	fmt.Fprintln(console.out, "Paired. That device can now reach this backend.")
	fmt.Fprintln(console.out, "Manage it, or add another, from Settings > Access.")
	return nil
}

// awaitRedemption polls one link until a device claims it or it settles.
//
// Polling rather than a subscription because there is nothing to subscribe
// to: redemption happens inside the session core on an HTTP request this
// process is serving, and the status read is one indexed row. A read
// failure ends the wait rather than retrying — the alternative is a loop
// that hides a broken store behind a spinner until the link expires.
func awaitRedemption(ctx context.Context, surface serveEnrollment, linkID string, poll time.Duration) (appservice.PairingStatusView, error) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		status, err := surface.Status(linkID)
		if err != nil {
			return appservice.PairingStatusView{}, err
		}
		if status.Redeemed() || status.Settled() {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return appservice.PairingStatusView{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// askYesNo prompts once and reads one line.
//
// Once, not until-valid: this runs on a backend that is already serving,
// and a re-prompt loop on a stdin that has gone away would spin forever.
// Anything that is not an explicit yes is a no, which is the safe default
// for a question whose yes enrolls a device.
func askYesNo(console serveConsole, prompt string) (bool, error) {
	fmt.Fprint(console.out, prompt)
	line, err := console.in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		// EOF with nothing typed is a closed stdin, not an answer. EOF
		// AFTER a word (a here-string, a pipe with no trailing newline) is
		// the answer somebody meant, so that case falls through.
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
