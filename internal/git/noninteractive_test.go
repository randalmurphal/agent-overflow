package git

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// credentialProbeGit logs "<GIT_TERMINAL_PROMPT>|<GIT_ASKPASS>|<SSH_ASKPASS>|
// <SSH_ASKPASS_REQUIRE>|<GCM_INTERACTIVE>|<argv>" per invocation and answers
// just enough for the Core entry points under test to reach their network
// command. A missing variable logs as "<unset>" so "inherited the user's
// value" and "explicitly blanked" stay distinguishable.
const credentialProbeGit = `#!/bin/sh
printf '%s|%s|%s|%s|%s|%s\n' \
  "${GIT_TERMINAL_PROMPT-<unset>}" \
  "${GIT_ASKPASS-<unset>}" \
  "${SSH_ASKPASS-<unset>}" \
  "${SSH_ASKPASS_REQUIRE-<unset>}" \
  "${GCM_INTERACTIVE-<unset>}" \
  "$*" >> "$AO_GIT_LOG"
case "$1 $2" in
  "rev-parse --show-toplevel") echo "/tmp/repo" ;;
  "rev-parse --git-common-dir") echo "/tmp/repo/.git" ;;
  "symbolic-ref --quiet") echo "feature" ;;
  "rev-parse --abbrev-ref") echo "origin/feature" ;;
  "remote ") echo "origin" ;;
esac
exit 0
`

// userAskpass is the askpass helper the tests pretend the user has
// configured. Its presence is the whole point: GIT_TERMINAL_PROMPT=0 alone
// does not defeat it (verified against git 2.43.0), so a command that only
// clears the terminal prompt would still pop this helper's GUI.
const userAskpass = "/usr/lib/ssh/user-askpass"

// pinUserCredentialEnv gives all five variables a distinctive value so the
// assertions below read the same on any host. Some CI images and shells
// already export GIT_TERMINAL_PROMPT / GCM_INTERACTIVE, and a test that
// asserted "inherited" against an unpinned environment would pass for the
// wrong reason there.
func pinUserCredentialEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GIT_ASKPASS", userAskpass)
	t.Setenv("SSH_ASKPASS", userAskpass)
	t.Setenv("SSH_ASKPASS_REQUIRE", "force")
	t.Setenv("GCM_INTERACTIVE", "auto")
	return "1|" + userAskpass + "|" + userAskpass + "|force|auto"
}

// commandCredentialEnv parses installMockGit's log into argv -> the joined
// credential-prompt environment.
func commandCredentialEnv(t *testing.T, logPath string) map[string]string {
	t.Helper()
	env := make(map[string]string)
	for _, line := range strings.Split(readFile(t, logPath), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 6)
		if len(parts) != 6 {
			t.Fatalf("malformed mock git log line %q", line)
		}
		env[parts[5]] = strings.Join(parts[:5], "|")
	}
	return env
}

// blockedEnv is what nonInteractiveEnv must look like from inside the child:
// prompts off, both askpass hooks blanked, ssh and GCM told never to ask.
const blockedEnv = "0|||never|never"

// TestCredentialPromptsBlockedUnlessUserInitiated pins both halves of the
// contract: nothing running on a watcher edge, a timer, or a picker's
// opportunistic warm-up can reach a credential prompt, and the commands a
// human is actively waiting on still can.
func TestCredentialPromptsBlockedUnlessUserInitiated(t *testing.T) {
	logPath := installMockGit(t, credentialProbeGit)
	wantInherited := pinUserCredentialEnv(t)

	core := NewCore()
	cwd := t.TempDir()

	if _, err := core.Status(cwd); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := core.MaybeFetchRemotes(cwd); err != nil {
		t.Fatalf("MaybeFetchRemotes: %v", err)
	}
	// The 5-minute cadence: the one caller that runs with nobody even
	// looking at the window. It shares its staleness clock with the
	// picker warm-up above, so drop the stamp that call just wrote or
	// this one correctly skips and never reaches git.
	core.InvalidateFetchCache(cwd)
	if _, err := core.FetchRemotesBackground(t.Context(), cwd); err != nil {
		t.Fatalf("FetchRemotesBackground: %v", err)
	}
	// The workflows engine's push: a network command with nobody watching.
	// Attended Push shares this argv, so it is asserted separately in
	// TestPushAttendedAndUnattendedShareOneArgv rather than here, where the
	// two would collapse onto one log key.
	if err := core.PushUnattended(cwd); err != nil {
		t.Fatalf("PushUnattended: %v", err)
	}

	if err := core.Pull(cwd); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if err := core.PruneRemotes(cwd); err != nil {
		t.Fatalf("PruneRemotes: %v", err)
	}
	if err := core.FetchBranch(cwd, "origin", "main"); err != nil {
		t.Fatalf("FetchBranch: %v", err)
	}

	env := commandCredentialEnv(t, logPath)

	// Background / automatic: no prompt channel survives.
	blocked := []string{
		"status --porcelain=v2 --branch",
		"fetch --all",
		"fetch --quiet origin",
		"push",
	}
	for _, argv := range blocked {
		got, ok := env[argv]
		if !ok {
			t.Fatalf("mock git never saw %q; log:\n%s", argv, readFile(t, logPath))
		}
		if got != blockedEnv {
			t.Errorf("`git %s` ran with credential env %q, want %q", argv, got, blockedEnv)
		}
	}

	// User-initiated: the user's own prompt configuration is left intact.
	allowed := []string{
		"pull --ff-only",
		"fetch --all --prune",
		"fetch origin main",
	}
	for _, argv := range allowed {
		got, ok := env[argv]
		if !ok {
			t.Fatalf("mock git never saw %q; log:\n%s", argv, readFile(t, logPath))
		}
		if got != wantInherited {
			t.Errorf("`git %s` ran with credential env %q, want %q", argv, got, wantInherited)
		}
	}
}

// TestPushAttendedAndUnattendedShareOneArgv is the transition half of the
// Push contract: the two entry points must differ ONLY in whether a prompt
// can appear, so a future change to push argv cannot drift between them.
func TestPushAttendedAndUnattendedShareOneArgv(t *testing.T) {
	logPath := installMockGit(t, credentialProbeGit)
	pinUserCredentialEnv(t)

	core := NewCore()
	cwd := t.TempDir()
	if err := core.Push(cwd); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := core.PushUnattended(cwd); err != nil {
		t.Fatalf("PushUnattended: %v", err)
	}

	var pushArgv []string
	for _, line := range strings.Split(readFile(t, logPath), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 6)
		if len(parts) != 6 {
			t.Fatalf("malformed mock git log line %q", line)
		}
		if strings.HasPrefix(parts[5], "push") {
			pushArgv = append(pushArgv, parts[5]+"\x00"+strings.Join(parts[:5], "|"))
		}
	}
	if len(pushArgv) != 2 {
		t.Fatalf("expected 2 push invocations, got %d; log:\n%s", len(pushArgv), readFile(t, logPath))
	}

	attendedArgv, attendedEnv, _ := strings.Cut(pushArgv[0], "\x00")
	unattendedArgv, unattendedEnv, _ := strings.Cut(pushArgv[1], "\x00")
	if attendedArgv != unattendedArgv {
		t.Errorf("push argv drifted: attended %q vs unattended %q", attendedArgv, unattendedArgv)
	}
	if attendedEnv == unattendedEnv {
		t.Errorf("attended and unattended push shared credential env %q", attendedEnv)
	}
	if unattendedEnv != blockedEnv {
		t.Errorf("PushUnattended credential env = %q, want %q", unattendedEnv, blockedEnv)
	}
}

// TestBackgroundFetchFailsFastInsteadOfAskingRealGit runs the real git
// binary against a local endpoint that demands HTTP basic auth, with an
// askpass helper the user "configured". Everything above this test asserts
// the environment we hand the child; this asserts what git actually does
// with it — that the background fetch never launches the helper and fails
// with a readable error, while the user-initiated prune does launch it.
func TestBackgroundFetchFailsFastInsteadOfAskingRealGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script askpass helper is unix-only")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	// A clean config chain: a real user's credential.helper would answer
	// before git ever reaches the askpass fallback this test is about.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", server.URL+"/repo.git")

	helperDir := t.TempDir()
	askpassLog := filepath.Join(helperDir, "askpass.log")
	askpass := filepath.Join(helperDir, "askpass.sh")
	script := "#!/bin/sh\necho \"invoked $*\" >> " + askpassLog + "\necho someone\n"
	if err := os.WriteFile(askpass, []byte(script), 0o755); err != nil {
		t.Fatalf("write askpass helper: %v", err)
	}
	t.Setenv("GIT_ASKPASS", askpass)

	core := NewCore()

	if _, err := core.MaybeFetchRemotes(repo); err == nil {
		t.Fatal("MaybeFetchRemotes against a credential-requiring remote should fail")
	}
	if _, err := os.Stat(askpassLog); err == nil {
		t.Fatalf("picker warm-up fetch invoked the askpass helper:\n%s", readFile(t, askpassLog))
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat askpass log: %v", err)
	}

	// The timer-driven cadence: same contract, and the one a user with a
	// private remote and no ambient credentials hits every window forever.
	// A failure leaves the staleness clock unstamped, so it is genuinely
	// running git here rather than skipping on the previous call's stamp.
	if _, err := core.FetchRemotesBackground(t.Context(), repo); err == nil {
		t.Fatal("FetchRemotesBackground against a credential-requiring remote should fail")
	}
	if _, err := os.Stat(askpassLog); err == nil {
		t.Fatalf("background fetch invoked the askpass helper:\n%s", readFile(t, askpassLog))
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat askpass log: %v", err)
	}

	// Control: the user-initiated prune runs the same `git fetch` and must
	// still reach the helper, or the opt-out is dead code.
	if err := core.PruneRemotes(repo); err == nil {
		t.Fatal("PruneRemotes against a rejecting remote should fail")
	}
	if _, err := os.Stat(askpassLog); err != nil {
		t.Fatalf("user-initiated prune never invoked the askpass helper: %v", err)
	}
}
