package tailnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tailscale.com/ipn"
)

// TestConstructingANodeOpensNothing pins the opt-in property the spec
// states: while the feature is off the app builds one struct and nothing
// else. No directory, no goroutine, no socket, no key.
func TestConstructingANodeOpensNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tsnet")
	node, err := New(Options{Dir: dir})
	if err != nil {
		t.Fatalf("build node: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the state directory exists after New alone (stat error %v); nothing may be written until Start", err)
	}
	if got := node.Status(); got.State != "" || got.Running() {
		t.Errorf("Status() = %+v on an unstarted node, want the zero snapshot", got)
	}
	if _, err := node.Listen(8080); err == nil {
		t.Error("Listen succeeded on a node that was never started")
	}
}

// TestNewRefusesWithoutAStateDirectory keeps the one mistake that would be
// invisible out of reach: a node with nowhere to keep its key enrolls a
// NEW device every time the app starts, filling the owner's admin panel
// with machines they cannot tell apart.
func TestNewRefusesWithoutAStateDirectory(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted an empty state directory")
	}
	if _, err := New(Options{Dir: "   "}); err == nil {
		t.Fatal("New accepted a blank state directory")
	}
}

// TestCloseIsSafeOnANodeThatNeverStarted is the guard the spike found the
// hard way: tsnet.Server.Close on a server that never ran Start panics.
// A disable arriving while an enable is still failing is exactly that
// shape, so it is a case rather than a comment.
func TestCloseIsSafeOnANodeThatNeverStarted(t *testing.T) {
	node, err := New(Options{Dir: filepath.Join(t.TempDir(), "tsnet")})
	if err != nil {
		t.Fatalf("build node: %v", err)
	}
	if err := node.Close(); err != nil {
		t.Fatalf("close an unstarted node: %v", err)
	}
	if err := node.Close(); err != nil {
		t.Fatalf("close twice: %v", err)
	}
	if err := node.Start(); err == nil {
		t.Error("a closed node started; Node is single use and a restart must build a new one")
	}
}

// TestForgetRemovesTheStateDirectory covers the other half of the on/off
// pair. Disabling keeps the identity; forgetting is the separate act that
// deletes it, which is how an owner moves this backend to another tailnet.
func TestForgetRemovesTheStateDirectory(t *testing.T) {
	root := t.TempDir()
	dir := StateDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("seed a state directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tailscaled.state"), []byte("node key"), 0o600); err != nil {
		t.Fatalf("seed node state: %v", err)
	}
	if err := Forget(root); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the state directory survived Forget (stat error %v)", err)
	}
	// Idempotent: forgetting what is already gone is not a failure, which
	// is what lets the app offer the action without first proving there is
	// something behind it.
	if err := Forget(root); err != nil {
		t.Errorf("forget an already-empty root: %v", err)
	}
	if err := Forget(""); err == nil {
		t.Error("Forget accepted an empty configuration root")
	}
}

// TestStatusPublishesTheSignInLinkAndClearsItOnJoin is the interactive
// path, which is the ONLY path the first time a backend joins. tsnet's Up
// never returns while a node waits for approval, so the link has to come
// off the status feed or the owner has no way to finish the enrollment.
func TestStatusPublishesTheSignInLinkAndClearsItOnJoin(t *testing.T) {
	requireBringUpCapableHost(t)
	testContext(t)

	controlURL, control := startControl(t)
	control.RequireAuth = true

	node := startTestNode(t, controlURL, "waits-for-approval")

	waiting := awaitStatus(t, node, "a sign-in link", func(s Status) bool { return s.AuthURL != "" })
	if waiting.State != ipn.NeedsLogin.String() {
		t.Errorf("state while waiting = %q, want %q", waiting.State, ipn.NeedsLogin)
	}
	if waiting.Running() {
		t.Error("the node reported Running while it was still waiting for approval")
	}
	// The listeners must refuse until the node has actually joined:
	// tsnet's ListenTLS waits for Running internally and would block here
	// forever with nothing saying why.
	if _, err := node.Listen(8080); err == nil {
		t.Error("Listen succeeded before the node joined the tailnet")
	} else if !strings.Contains(err.Error(), ipn.NeedsLogin.String()) {
		t.Errorf("Listen refused with %q, which does not say what the node is waiting for", err)
	}
	if _, err := node.ListenTLS(); err == nil {
		t.Error("ListenTLS succeeded before the node joined the tailnet")
	}

	if !control.CompleteAuth(waiting.AuthURL) {
		t.Fatalf("the fake control server did not recognise the link it issued: %q", waiting.AuthURL)
	}

	joined := awaitRunning(t, node)
	if joined.AuthURL != "" {
		t.Errorf("the sign-in link is still published after joining: %q", joined.AuthURL)
	}
	if !strings.HasSuffix(joined.DNSName, ".test-tailnet.ts.net") {
		t.Errorf("DNSName = %q, want the node's name under the tailnet's MagicDNS domain", joined.DNSName)
	}
	if strings.HasSuffix(joined.DNSName, ".") {
		t.Errorf("DNSName = %q still carries the trailing root dot, which no URL may contain", joined.DNSName)
	}
}

// TestTurningTheNodeOffAndOnAgainKeepsItsIdentity is the transition the
// reconciler performs and the one a user notices getting wrong: toggling
// the feature must not enroll a second device. Node is single use, so an
// on/off/on cycle in one process is three constructions over one
// directory — which is exactly what this exercises.
func TestTurningTheNodeOffAndOnAgainKeepsItsIdentity(t *testing.T) {
	requireBringUpCapableHost(t)
	testContext(t)

	controlURL, _ := startControl(t)
	dir := filepath.Join(t.TempDir(), "tsnet")

	first := newTestNode(t, controlURL, "steady-identity", dir)
	if err := first.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	joined := awaitRunning(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A second close is what a shutdown racing a disable does.
	if err := first.Close(); err != nil {
		t.Fatalf("close twice: %v", err)
	}

	// The identity is on disk, and it is 0700 because it holds this
	// backend's place on the owner's tailnet.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("the state directory is gone after a disable: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("the state directory is mode %v; it holds private key material", perm)
	}

	second := newTestNode(t, controlURL, "steady-identity", dir)
	t.Cleanup(func() { _ = second.Close() })
	if err := second.Start(); err != nil {
		t.Fatalf("restart on the same directory: %v", err)
	}
	rejoined := awaitRunning(t, second)

	if rejoined.DNSName != joined.DNSName {
		t.Errorf("DNSName changed across an off/on cycle: %q then %q; the owner would see a second device",
			joined.DNSName, rejoined.DNSName)
	}
	if strings.Join(rejoined.IPs, ",") != strings.Join(joined.IPs, ",") {
		t.Errorf("tailnet addresses changed across an off/on cycle: %v then %v", joined.IPs, rejoined.IPs)
	}
	if rejoined.AuthURL != "" {
		t.Errorf("the node asked for approval again after a restart: %q", rejoined.AuthURL)
	}
}
