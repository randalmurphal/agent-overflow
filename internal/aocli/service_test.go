package aocli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/supervise"
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
		configRoot: filepath.Join(home, "config", "agent-overflow"),
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
	// The unit starts the SUPERVISOR, not the backend directly: it is what
	// owns which version runs, and it spawns `serve` itself.
	if !strings.Contains(string(body), "ExecStart="+env.executable+" supervise\n") {
		t.Errorf("the unit does not start the supervisor:\n%s", body)
	}

	for _, want := range []string{"Installed the Agent Overflow service.", unit, env.executable + " supervise"} {
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
	if !strings.Contains(string(body), "supervise --listen 0.0.0.0:7777\n") {
		t.Errorf("the unit does not carry --listen:\n%s", body)
	}
	if !strings.Contains(stdout, "supervise --listen 0.0.0.0:7777") {
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
	if !strings.Contains(string(body), "ExecStart=/opt/agent-overflow/current supervise\n") {
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

// `service update` is the LOCAL update path: the operator is standing there, so
// there is no trial and no rollback. What it must do is stop the unit, stage
// the binary under its own reported version, select it, and start the unit
// again — in that order, because staging into a version directory while the
// supervisor is running would race the file it may be about to spawn.
func TestServiceUpdateStagesTheBinaryAndRestartsTheUnit(t *testing.T) {
	runner := &recordingRunner{}
	env := testServiceEnv(t, runner)
	writeFakeBinary(t, env.executable, "9.9.9")
	runner.answer(env.executable+" "+supervise.PreflightSubcommand,
		`{"protocolVersion":1,"version":"9.9.9"}`, 0)

	code, stdout, stderr := runService(t, env, "update")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "version 9.9.9") {
		t.Errorf("stdout does not name the installed version:\n%s", stdout)
	}

	// The staged copy is the operator's binary, under the version it reported.
	layout, err := supervise.NewLayout(env.configRoot)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	staged, err := layout.VersionBinary("9.9.9")
	if err != nil {
		t.Fatalf("VersionBinary: %v", err)
	}
	source, err := os.ReadFile(env.executable)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	copied, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if string(copied) != string(source) {
		t.Error("the staged file is not the binary that was installed")
	}

	// And it is what the supervisor will select on its next boot.
	state, found, err := supervise.LoadState(layout)
	if err != nil || !found {
		t.Fatalf("LoadState = (found %t, %v)", found, err)
	}
	if state.ActiveVersion != "9.9.9" || state.Update != nil {
		t.Fatalf("state = %+v, want 9.9.9 active with no update in flight", state)
	}

	// Stop before start, with the staging between them.
	var stopAt, startAt = -1, -1
	for i, call := range runner.calls {
		switch {
		case strings.Contains(call, " stop "):
			stopAt = i
		case strings.Contains(call, " start "):
			startAt = i
		}
	}
	if stopAt < 0 || startAt < 0 || stopAt > startAt {
		t.Fatalf("calls = %v, want a stop before a start", runner.calls)
	}
}

// A binary that cannot answer the preflight is not one this can install, and
// the refusal happens BEFORE the unit is stopped: an operator whose new binary
// is broken must still have the running one.
func TestServiceUpdateRefusesABinaryThatDoesNotAnswerAndLeavesTheUnitRunning(t *testing.T) {
	runner := &recordingRunner{}
	env := testServiceEnv(t, runner)
	writeFakeBinary(t, env.executable, "unused")
	runner.answer(env.executable+" "+supervise.PreflightSubcommand, "not an agent-overflow binary", 127)

	code, _, stderr := runService(t, env, "update")
	if code == exitOK {
		t.Fatal("update accepted a binary that could not answer the preflight")
	}
	if !strings.Contains(stderr, supervise.PreflightSubcommand) {
		t.Errorf("stderr does not say what was asked:\n%s", stderr)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " stop ") {
			t.Fatalf("the unit was stopped for an update that was refused: %v", runner.calls)
		}
	}
	layout, err := supervise.NewLayout(env.configRoot)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if _, found, err := supervise.LoadState(layout); err != nil || found {
		t.Fatalf("a refused update wrote launch state (found %t, %v)", found, err)
	}
}

// A version string that cannot name a directory is refused with the version in
// the message, because the operator's own build stamped it.
func TestServiceUpdateRefusesAVersionThatCannotNameADirectory(t *testing.T) {
	runner := &recordingRunner{}
	env := testServiceEnv(t, runner)
	writeFakeBinary(t, env.executable, "unused")
	runner.answer(env.executable+" "+supervise.PreflightSubcommand,
		`{"protocolVersion":1,"version":"../escape"}`, 0)

	code, _, stderr := runService(t, env, "update")
	if code == exitOK {
		t.Fatal("update accepted a version that escapes the versions directory")
	}
	if !strings.Contains(stderr, "../escape") {
		t.Errorf("stderr does not name the version it refused:\n%s", stderr)
	}
}

// `service update` is documented where every other verb is, so an operator
// finds it without reading source.
func TestServiceUpdateIsDocumented(t *testing.T) {
	if !strings.Contains(serviceUsage, "update") {
		t.Errorf("the service usage does not list update:\n%s", serviceUsage)
	}
	if !strings.Contains(serviceUpdateUsage, "supervisor") {
		t.Errorf("the update usage does not explain what it replaces:\n%s", serviceUpdateUsage)
	}
}

// writeFakeBinary is a file to be COPIED, never one to be run: every command
// this package issues goes through the injected Runner.
func writeFakeBinary(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("fake agent-overflow "+marker), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestServiceStartStopPreserveInstalledUnit(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			runner := &recordingRunner{}
			env := testServiceEnv(t, runner)
			env.goos = goos
			if code, _, stderr := runService(t, env, "install"); code != exitOK {
				t.Fatal(stderr)
			}
			manager, _, _ := serviceManager(env, "", "", &bytes.Buffer{})
			before, err := os.ReadFile(manager.UnitPath())
			if err != nil {
				t.Fatal(err)
			}
			for _, action := range []string{"stop", "start"} {
				if code, _, stderr := runService(t, env, action); code != exitOK {
					t.Fatalf("%s: %s", action, stderr)
				}
			}
			after, err := os.ReadFile(manager.UnitPath())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("service control changed the unit: %v", err)
			}
		})
	}
}

func TestServiceStartExplainsMissingInstallation(t *testing.T) {
	env := testServiceEnv(t, &recordingRunner{})
	code, _, stderr := runService(t, env, "start")
	if code == exitOK || !strings.Contains(stderr, "service install") {
		t.Fatalf("start = %d, %s", code, stderr)
	}
}
