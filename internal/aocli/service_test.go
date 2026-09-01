package aocli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingRunner answers every service-manager command without running one.
// hostServiceEnv is never called in this file: a test that reached the real
// machine would enable a service on the developer's own login.
type recordingRunner struct {
	calls   []string
	answers map[string]struct {
		output string
		code   int
	}
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) (string, int, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, key)
	if a, ok := r.answers[key]; ok {
		return a.output, a.code, nil
	}
	return "", 0, nil
}

func (r *recordingRunner) answer(key, output string, code int) {
	if r.answers == nil {
		r.answers = map[string]struct {
			output string
			code   int
		}{}
	}
	r.answers[key] = struct {
		output string
		code   int
	}{output, code}
}

func testServiceEnv(t *testing.T, runner *recordingRunner) serviceEnv {
	t.Helper()
	home := t.TempDir()
	return serviceEnv{
		goos:       "linux",
		executable: filepath.Join(home, "bin", "agent-overflow"),
		home:       home,
		uid:        "1000",
		runner:     runner,
	}
}

func runService(t *testing.T, env serviceEnv, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = serviceCommand(args, env, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestServiceIsATopLevelCommand(t *testing.T) {
	if !IsCommand("service") {
		t.Fatal("service is not a top-level command")
	}
	var found bool
	for _, name := range Commands() {
		if name == "service" {
			found = true
		}
	}
	if !found {
		t.Error("Commands() does not name service")
	}
	if !strings.Contains(rootUsage, "service install") {
		t.Error("the root usage does not name the service command")
	}
}

func TestServiceWithNoCommandPrintsUsage(t *testing.T) {
	code, stdout, stderr := runService(t, testServiceEnv(t, &recordingRunner{}))
	if code != exitError {
		t.Errorf("exit = %d, want %d", code, exitError)
	}
	if stdout != "" {
		t.Errorf("usage went to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "Usage: agent-overflow service") {
		t.Errorf("stderr does not carry the usage: %q", stderr)
	}
}

func TestServiceHelpGoesToStdout(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		code, stdout, _ := runService(t, testServiceEnv(t, &recordingRunner{}), arg)
		if code != exitOK {
			t.Errorf("%s exit = %d, want %d", arg, code, exitOK)
		}
		if !strings.Contains(stdout, "Usage: agent-overflow service") {
			t.Errorf("%s printed no usage: %q", arg, stdout)
		}
	}
}

func TestServiceRefusesAnUnknownSubcommand(t *testing.T) {
	code, _, stderr := runService(t, testServiceEnv(t, &recordingRunner{}), "restart")
	if code != exitError {
		t.Errorf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, `unknown command "restart"`) {
		t.Errorf("stderr does not name the unknown command: %q", stderr)
	}
}

func TestServiceInstallWritesTheUnitAndSaysWhatItDid(t *testing.T) {
	runner := &recordingRunner{}
	env := testServiceEnv(t, runner)

	code, stdout, stderr := runService(t, env, "install")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}

	unit := filepath.Join(env.home, ".config", "systemd", "user", "agent-overflow.service")
	body, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("read the unit: %v", err)
	}
	if !strings.Contains(string(body), "ExecStart="+env.executable+" serve\n") {
		t.Errorf("the unit does not start serve:\n%s", body)
	}

	for _, want := range []string{"Installed the Agent Overflow service.", unit, env.executable + " serve"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not mention %q:\n%s", want, stdout)
		}
	}
	// Lingering is named, never run.
	if !strings.Contains(stdout, "loginctl enable-linger") {
		t.Errorf("stdout does not explain lingering:\n%s", stdout)
	}
	if len(runner.calls) != 2 {
		t.Errorf("commands = %v, want daemon-reload then enable", runner.calls)
	}
}

func TestServiceInstallCarriesTheListenFlagIntoTheUnit(t *testing.T) {
	env := testServiceEnv(t, &recordingRunner{})
	code, stdout, stderr := runService(t, env, "install", "--listen", "0.0.0.0:7777")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	unit := filepath.Join(env.home, ".config", "systemd", "user", "agent-overflow.service")
	body, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("read the unit: %v", err)
	}
	if !strings.Contains(string(body), "serve --listen 0.0.0.0:7777\n") {
		t.Errorf("the unit does not carry --listen:\n%s", body)
	}
	if !strings.Contains(stdout, "serve --listen 0.0.0.0:7777") {
		t.Errorf("stdout does not echo the command it installed:\n%s", stdout)
	}
}

// --binary exists because the default resolves symlinks: an operator who
// upgrades by replacing a symlink target needs the service to name the
// symlink, not today's file.
func TestServiceInstallHonorsAnExplicitBinary(t *testing.T) {
	env := testServiceEnv(t, &recordingRunner{})
	code, _, stderr := runService(t, env, "install", "--binary", "/opt/agent-overflow/current")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	body, err := os.ReadFile(filepath.Join(env.home, ".config", "systemd", "user", "agent-overflow.service"))
	if err != nil {
		t.Fatalf("read the unit: %v", err)
	}
	if !strings.Contains(string(body), "ExecStart=/opt/agent-overflow/current serve\n") {
		t.Errorf("the unit does not start the named binary:\n%s", body)
	}
}

func TestServiceInstallRefusesARelativeBinary(t *testing.T) {
	code, _, stderr := runService(t, testServiceEnv(t, &recordingRunner{}), "install", "--binary", "agent-overflow")
	if code != exitError {
		t.Errorf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "absolute") {
		t.Errorf("stderr does not explain the refusal: %q", stderr)
	}
}

func TestServiceUninstallSaysWhatItLeftAlone(t *testing.T) {
	env := testServiceEnv(t, &recordingRunner{})
	if code, _, stderr := runService(t, env, "install"); code != exitOK {
		t.Fatalf("install: exit = %d (stderr: %s)", code, stderr)
	}

	code, stdout, stderr := runService(t, env, "uninstall")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Removed the Agent Overflow service") {
		t.Errorf("stdout does not confirm the removal:\n%s", stdout)
	}
	// An operator running uninstall must not have to wonder about their data.
	if !strings.Contains(stdout, "credentials are still there") {
		t.Errorf("stdout does not say the data survives:\n%s", stdout)
	}
	unit := filepath.Join(env.home, ".config", "systemd", "user", "agent-overflow.service")
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Errorf("the unit survived: %v", err)
	}
}

// status is meant to read in a shell conditional, so "not running" is an
// answer with a non-zero exit rather than an error.
func TestServiceStatusExitsOnWhetherItIsRunning(t *testing.T) {
	cases := []struct {
		name     string
		active   string
		code     int
		wantExit int
		wantLine string
	}{
		{name: "running", active: "active", wantExit: exitOK, wantLine: "Running:   yes (active)"},
		{name: "stopped", active: "inactive", code: 3, wantExit: exitFindings, wantLine: "Running:   no (inactive)"},
		{name: "crashed", active: "failed", code: 3, wantExit: exitFindings, wantLine: "Running:   no (failed)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{}
			runner.answer("systemctl --user is-enabled agent-overflow.service", "enabled", 0)
			runner.answer("systemctl --user is-active agent-overflow.service", tc.active, tc.code)

			code, stdout, stderr := runService(t, testServiceEnv(t, runner), "status")
			if code != tc.wantExit {
				t.Errorf("exit = %d, want %d (stderr: %s)", code, tc.wantExit, stderr)
			}
			if !strings.Contains(stdout, tc.wantLine) {
				t.Errorf("stdout does not carry %q:\n%s", tc.wantLine, stdout)
			}
			if !strings.Contains(stdout, "Starts up: yes") {
				t.Errorf("stdout does not report the enabled state:\n%s", stdout)
			}
		})
	}
}

// Windows is not a failure of this machine, it is an answer about it — and the
// answer has to name the thing that IS the supervisor there.
func TestServiceOnWindowsNamesTheLauncher(t *testing.T) {
	env := testServiceEnv(t, &recordingRunner{})
	env.goos = "windows"
	for _, command := range []string{"install", "uninstall", "status"} {
		code, _, stderr := runService(t, env, command)
		if code != exitFindings {
			t.Errorf("%s exit = %d, want %d", command, code, exitFindings)
		}
		if !strings.Contains(stderr, "WSL") {
			t.Errorf("%s does not name WSL: %q", command, stderr)
		}
	}
}

func TestServiceReportsAHostItCannotDescribe(t *testing.T) {
	env := testServiceEnv(t, &recordingRunner{})
	env.home = ""
	env.homeErr = os.ErrNotExist
	code, _, stderr := runService(t, env, "status")
	if code != exitError {
		t.Errorf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "home directory") {
		t.Errorf("stderr does not name the missing fact: %q", stderr)
	}
}

// --help must work even where os.Executable() does not, which is why the env's
// two failures are carried rather than resolved eagerly.
func TestServiceSubcommandHelpWorksWithoutAResolvableBinary(t *testing.T) {
	env := testServiceEnv(t, &recordingRunner{})
	env.executable = ""
	env.executableErr = os.ErrNotExist
	code, stdout, _ := runService(t, env, "install", "--help")
	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "Usage: agent-overflow service install") {
		t.Errorf("no usage printed:\n%s", stdout)
	}
}

func TestServiceRefusesPositionalArguments(t *testing.T) {
	for _, command := range []string{"install", "uninstall", "status"} {
		code, _, stderr := runService(t, testServiceEnv(t, &recordingRunner{}), command, "extra")
		if code != exitError {
			t.Errorf("%s exit = %d, want %d", command, code, exitError)
		}
		if !strings.Contains(stderr, "unexpected positional arguments") {
			t.Errorf("%s stderr = %q", command, stderr)
		}
	}
}
