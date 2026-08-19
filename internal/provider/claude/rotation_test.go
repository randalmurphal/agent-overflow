package claude

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func credentialExpiring(in time.Duration) []byte {
	return fmt.Appendf(nil,
		`{"claudeAiOauth":{"accessToken":"at","refreshToken":"rt","expiresAt":%d}}`,
		time.Now().Add(in).UnixMilli(),
	)
}

// writeRotatingClaudeScript emulates the one CLI behaviour that destroys
// logins: the token refresh is a DETACHED task, the initialize response is
// answered without awaiting it, and under `--max-turns 0` the process exits
// the moment stdin closes — taking the unwritten rotation with it.
//
// The mock answers immediately, starts a writer that lands the rotated
// credential after delay, and kills that writer when its stdin closes. A probe
// that tears down on the answer therefore loses the rotation exactly as the
// real CLI does.
func writeRotatingClaudeScript(t *testing.T, dir, credentialPath string, delay time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, "claude-rotating")
	response := `{"type":"control_response","response":{"subtype":"success","request_id":"` +
		probeInitRequestID + `","response":{"account":{"email":"probe@example.test"}}}}`
	script := "#!/bin/bash\n" +
		"read -r _ || true\n" +
		`printf '%s\n' '` + response + `'` + "\n" +
		fmt.Sprintf("( sleep %.2f; printf 'rotated' > %q ) &\n", delay.Seconds(), credentialPath) +
		"writer=$!\n" +
		"read -r _ || true\n" +
		`kill "$writer" 2>/dev/null` + "\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	return path
}

// The headline regression. Spawning the CLI against a credential at or past
// expiry starts a rotation the CLI has not finished when it answers; tearing
// the probe down on that answer loses the replacement pair permanently,
// because the server retired the old refresh token the moment it was spent.
func TestProbeAccountWaitsForAnExpectedRotationToLand(t *testing.T) {
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credentialPath, credentialExpiring(-time.Hour), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := writeRotatingClaudeScript(t, dir, credentialPath, 400*time.Millisecond)

	if _, err := ProbeAccount(context.Background(), ProbeConfig{
		WorkDir:        probeWorkDir,
		Binary:         binary,
		ReadCredential: func() ([]byte, error) { return os.ReadFile(credentialPath) },
	}); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}

	data, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "rotated" {
		t.Fatalf("teardown killed the CLI before its rotation reached disk: credential = %q", data)
	}
}

// The gate's other half: a credential nowhere near expiry makes the CLI do no
// token work, so the probe must not pay the teardown wait. The mock's writer
// is killed at stdin close, so a probe that returned promptly leaves the
// credential untouched.
func TestProbeAccountDoesNotWaitWhenNoRotationIsExpected(t *testing.T) {
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, ".credentials.json")
	original := credentialExpiring(4 * time.Hour)
	if err := os.WriteFile(credentialPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	binary := writeRotatingClaudeScript(t, dir, credentialPath, 5*time.Second)

	started := time.Now()
	if _, err := ProbeAccount(context.Background(), ProbeConfig{
		WorkDir:        probeWorkDir,
		Binary:         binary,
		ReadCredential: func() ([]byte, error) { return os.ReadFile(credentialPath) },
	}); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("probe waited %s for a rotation that could not happen", elapsed)
	}

	data, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(original) {
		t.Fatalf("credential changed under a probe that expected no rotation: %q", data)
	}
}

// A probe that failed can have started a rotation just as easily as one that
// answered, so the settle runs on every exit path — here the CLI answers with
// a non-success subtype and the probe returns an error.
func TestProbeAccountWaitsForRotationEvenWhenTheProbeFails(t *testing.T) {
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credentialPath, credentialExpiring(-time.Hour), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "claude-failing")
	response := `{"type":"control_response","response":{"subtype":"error","request_id":"` +
		probeInitRequestID + `","error":"nope"}}`
	script := "#!/bin/bash\n" +
		"read -r _ || true\n" +
		`printf '%s\n' '` + response + `'` + "\n" +
		fmt.Sprintf("( sleep 0.40; printf 'rotated' > %q ) &\n", credentialPath) +
		"writer=$!\n" +
		"read -r _ || true\n" +
		`kill "$writer" 2>/dev/null` + "\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := ProbeAccount(context.Background(), ProbeConfig{
		WorkDir:        probeWorkDir,
		Binary:         path,
		ReadCredential: func() ([]byte, error) { return os.ReadFile(credentialPath) },
	}); err == nil {
		t.Fatal("expected the error subtype to fail the probe")
	}

	data, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "rotated" {
		t.Fatalf("a failed probe still killed the CLI mid-rotation: credential = %q", data)
	}
}

// A cancelled caller must not become a kill signal while a rotation is in
// flight: cancellation is exactly the case the budget exists for. The read
// still aborts promptly — only the teardown waits.
func TestProbeAccountLetsARotationLandAfterCallerCancellation(t *testing.T) {
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credentialPath, credentialExpiring(-time.Hour), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "claude-silent")
	// Never answers, so only the cancellation ends the probe.
	script := "#!/bin/bash\n" +
		"read -r _ || true\n" +
		fmt.Sprintf("( sleep 0.40; printf 'rotated' > %q ) &\n", credentialPath) +
		"writer=$!\n" +
		"read -r _ || true\n" +
		`kill "$writer" 2>/dev/null` + "\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	if _, err := ProbeAccount(ctx, ProbeConfig{
		WorkDir:        probeWorkDir,
		Binary:         path,
		ReadCredential: func() ([]byte, error) { return os.ReadFile(credentialPath) },
	}); err == nil {
		t.Fatal("expected the cancelled probe to fail")
	}

	data, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "rotated" {
		t.Fatalf("cancellation killed the CLI mid-rotation: credential = %q", data)
	}
}

func TestArmRotationWatchArmsOnlyWhenARotationIsExpected(t *testing.T) {
	now := time.Now()
	read := func(data []byte) func() ([]byte, error) {
		return func() ([]byte, error) { return data, nil }
	}
	fails := func(err error) func() ([]byte, error) {
		return func() ([]byte, error) { return nil, err }
	}
	cases := []struct {
		name         string
		read         func() ([]byte, error)
		expected     bool
		wantArmed    bool
		wantBaseline bool
	}{
		{name: "no reader"},
		{name: "already expired", read: read(credentialExpiring(-time.Hour)), wantArmed: true, wantBaseline: true},
		{name: "inside the refresh buffer", read: read(credentialExpiring(time.Minute)), wantArmed: true, wantBaseline: true},
		{name: "outside the refresh buffer", read: read(credentialExpiring(time.Hour))},
		{name: "no oauth object", read: read([]byte(`{"other":{}}`))},
		{name: "husk", read: read([]byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`))},
		{name: "unparseable", read: read([]byte("not json"))},
		// Absent and unreadable have opposite safe answers: nothing to
		// rotate versus cannot see whether something is rotating.
		{name: "absent", read: fails(fs.ErrNotExist)},
		{name: "absent, wrapped", read: fails(fmt.Errorf("keychain: %w", fs.ErrNotExist))},
		{name: "unreadable arms blind", read: fails(errors.New("keychain locked")), wantArmed: true},
		// The caller's own knowledge overrides bytes that look fresh: the
		// CLI's 401 recovery forces a refresh past every expiry gate.
		{
			name:         "forced despite a fresh expiry",
			read:         read(credentialExpiring(4 * time.Hour)),
			expected:     true,
			wantArmed:    true,
			wantBaseline: true,
		},
		{name: "forced with no credential at all", read: fails(fs.ErrNotExist), expected: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			watch := armRotationWatch(tc.read, tc.expected, now)
			if watch.armed != tc.wantArmed {
				t.Fatalf("armed = %v, want %v", watch.armed, tc.wantArmed)
			}
			if watch.baseline != tc.wantBaseline {
				t.Fatalf("baseline = %v, want %v", watch.baseline, tc.wantBaseline)
			}
			if (watch.budget() > 0) != tc.wantArmed {
				t.Fatalf("budget = %v with armed=%v", watch.budget(), watch.armed)
			}
		})
	}
}

// The branch that would silently switch the protection off: a credential that
// becomes unreadable DURING the wait is the moment the CLI is most likely to
// be mid-write, so the watch must keep waiting rather than treat the failure
// as "nothing to wait for".
func TestRotationWatchSettleKeepsWaitingThroughReadErrors(t *testing.T) {
	var mu sync.Mutex
	current := credentialExpiring(-time.Hour)
	// The arming read succeeds, establishing the baseline; the polls that
	// follow fail until the rotation lands.
	arming := true
	failing := false
	watch := armRotationWatch(func() ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if arming {
			arming, failing = false, true
			return current, nil
		}
		if failing {
			return nil, errors.New("keychain locked")
		}
		return current, nil
	}, false, time.Now())
	if !watch.armed || !watch.baseline {
		t.Fatalf("watch did not arm with a baseline: armed=%v baseline=%v", watch.armed, watch.baseline)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		current = credentialExpiring(8 * time.Hour)
		failing = false
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watch.settle(ctx)
	if ctx.Err() != nil {
		t.Fatal("settle waited out its deadline instead of seeing the write land after the read errors")
	}
}

// A blind watch cannot end early, but it must still hold the process — and it
// must still stop at the deadline.
func TestRotationWatchBlindSettleHoldsForTheWholeBudget(t *testing.T) {
	watch := armRotationWatch(
		func() ([]byte, error) { return nil, errors.New("keychain locked") },
		false,
		time.Now(),
	)
	if !watch.armed || watch.baseline {
		t.Fatalf("expected a blind arm: armed=%v baseline=%v", watch.armed, watch.baseline)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	watch.settle(ctx)
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("blind settle returned after %s instead of holding for its budget", elapsed)
	}
}

func TestRotationWatchSettleReturnsWhenTheCredentialChanges(t *testing.T) {
	var mu sync.Mutex
	current := credentialExpiring(-time.Hour)
	watch := armRotationWatch(func() ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	}, false, time.Now())
	if !watch.armed {
		t.Fatal("watch did not arm on an expired credential")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		current = credentialExpiring(8 * time.Hour)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	watch.settle(ctx)
	if ctx.Err() != nil {
		t.Fatalf("settle waited out its deadline after %s instead of seeing the write", time.Since(started))
	}
}

func TestRotationWatchSettleGivesUpAtItsDeadline(t *testing.T) {
	current := credentialExpiring(-time.Hour)
	watch := armRotationWatch(func() ([]byte, error) { return current, nil }, false, time.Now())
	if !watch.armed {
		t.Fatal("watch did not arm on an expired credential")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	watch.settle(ctx)
	if ctx.Err() == nil {
		t.Fatal("settle returned before its deadline with the credential unchanged")
	}
}

// An unarmed watch must be free: no reads, no wait.
func TestRotationWatchSettleIsInertWhenUnarmed(t *testing.T) {
	reads := 0
	watch := armRotationWatch(func() ([]byte, error) {
		reads++
		return credentialExpiring(time.Hour), nil
	}, false, time.Now())
	if watch.armed {
		t.Fatal("watch armed on a credential nowhere near expiry")
	}
	before := reads
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	watch.settle(ctx)
	if reads != before {
		t.Fatalf("an unarmed settle read the credential %d times", reads-before)
	}
	if ctx.Err() != nil {
		t.Fatal("an unarmed settle waited")
	}
}
