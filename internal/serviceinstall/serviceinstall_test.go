package serviceinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records every command and answers from a scripted table. No test
// in this package may construct ExecRunner: the whole point of the injected
// Runner is that `go test` cannot enable a service on the machine running it.
type fakeRunner struct {
	calls   []string
	answers map[string]answer
	fail    map[string]error
}

type answer struct {
	output string
	code   int
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) (string, int, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, key)
	if err, ok := r.fail[key]; ok {
		return "", -1, err
	}
	if a, ok := r.answers[key]; ok {
		return a.output, a.code, nil
	}
	return "", 0, nil
}

func newRunner() *fakeRunner {
	return &fakeRunner{answers: map[string]answer{}, fail: map[string]error{}}
}

func linuxConfig(home string) Config {
	return Config{GOOS: "linux", Executable: "/home/ada/.local/bin/agent-overflow", HomeDir: home}
}

func darwinConfig(home string) Config {
	return Config{
		GOOS:       "darwin",
		Executable: "/Users/ada/.local/bin/agent-overflow",
		HomeDir:    home,
		UID:        "501",
	}
}

func mustNew(t *testing.T, config Config, runner Runner) Manager {
	t.Helper()
	manager, err := New(config, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return manager
}

func mustContents(t *testing.T, manager Manager) string {
	t.Helper()
	contents, err := manager.UnitContents()
	if err != nil {
		t.Fatalf("UnitContents: %v", err)
	}
	return contents
}

// The unit files are golden-tested WHOLE, on any host, because that is the
// only way the launchd plist is ever read by someone who is not at a Mac. A
// diff here is a deliberate change to what gets installed on somebody's
// machine, so it should be seen in full.

const goldenSystemdUnit = `[Unit]
Description=Agent Overflow backend
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=120
StartLimitBurst=5

[Service]
Type=simple
ExecStart=/home/ada/.local/bin/agent-overflow supervise
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

const goldenLaunchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.agentoverflow.serve</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/ada/.local/bin/agent-overflow</string>
		<string>supervise</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>/Users/ada/Library/Logs/agent-overflow-serve.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/ada/Library/Logs/agent-overflow-serve.log</string>
</dict>
</plist>
`

func TestSystemdUnitIsGolden(t *testing.T) {
	manager := mustNew(t, linuxConfig("/home/ada"), newRunner())
	if got := mustContents(t, manager); got != goldenSystemdUnit {
		t.Errorf("systemd unit changed:\n--- got ---\n%s\n--- want ---\n%s", got, goldenSystemdUnit)
	}
	if want := "/home/ada/.config/systemd/user/agent-overflow.service"; manager.UnitPath() != want {
		t.Errorf("UnitPath = %q, want %q", manager.UnitPath(), want)
	}
}

func TestLaunchdPlistIsGolden(t *testing.T) {
	manager := mustNew(t, darwinConfig("/Users/ada"), newRunner())
	if got := mustContents(t, manager); got != goldenLaunchdPlist {
		t.Errorf("launchd plist changed:\n--- got ---\n%s\n--- want ---\n%s", got, goldenLaunchdPlist)
	}
	if want := "/Users/ada/Library/LaunchAgents/com.agentoverflow.serve.plist"; manager.UnitPath() != want {
		t.Errorf("UnitPath = %q, want %q", manager.UnitPath(), want)
	}
}

// The label must not be the app bundle's identifier: a Mac can run the desktop
// app and a serve agent at once, and launchd tells services apart by label.
func TestLaunchdLabelIsNotTheAppBundleIdentifier(t *testing.T) {
	if LaunchdLabel == "com.agentoverflow.app" {
		t.Fatal("the LaunchAgent label collides with the desktop app bundle identifier")
	}
}

func TestListenFlagReachesBothUnits(t *testing.T) {
	linux := linuxConfig("/home/ada")
	linux.Listen = "0.0.0.0:7777"
	if got := mustContents(t, mustNew(t, linux, newRunner())); !strings.Contains(got,
		"ExecStart=/home/ada/.local/bin/agent-overflow supervise --listen 0.0.0.0:7777\n") {
		t.Errorf("systemd ExecStart missing the listen flag:\n%s", got)
	}

	darwin := darwinConfig("/Users/ada")
	darwin.Listen = "0.0.0.0:7777"
	got := mustContents(t, mustNew(t, darwin, newRunner()))
	for _, want := range []string{"<string>--listen</string>", "<string>0.0.0.0:7777</string>"} {
		if !strings.Contains(got, want) {
			t.Errorf("launchd plist missing %s:\n%s", want, got)
		}
	}
}

// systemd's `%` is a specifier introducer and its whitespace splits argv, so a
// home directory that contains either must not silently produce a unit that
// starts something else.
func TestSystemdQuotesPathsThatWouldOtherwiseChangeMeaning(t *testing.T) {
	cases := []struct {
		name string
		exe  string
		want string
	}{
		{name: "plain", exe: "/opt/ao/agent-overflow", want: "ExecStart=/opt/ao/agent-overflow supervise"},
		{name: "a space", exe: "/opt/my apps/agent-overflow", want: `ExecStart="/opt/my apps/agent-overflow" supervise`},
		{name: "a percent", exe: "/opt/100%/agent-overflow", want: "ExecStart=/opt/100%%/agent-overflow supervise"},
		{name: "a quote", exe: `/opt/a"b/agent-overflow`, want: `ExecStart="/opt/a\"b/agent-overflow" supervise`},
		{name: "a backslash", exe: `/opt/a\b/agent-overflow`, want: `ExecStart="/opt/a\\b/agent-overflow" supervise`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := linuxConfig("/home/ada")
			config.Executable = tc.exe
			got := mustContents(t, mustNew(t, config, newRunner()))
			if !strings.Contains(got, tc.want+"\n") {
				t.Errorf("ExecStart is not %q:\n%s", tc.want, got)
			}
		})
	}
}

// A newline cannot be represented in a unit file value at all, so generation
// refuses rather than writing a file that means something else.
func TestSystemdRefusesANewlineInThePath(t *testing.T) {
	config := linuxConfig("/home/ada")
	config.Executable = "/opt/a\nb/agent-overflow"
	if _, err := mustNew(t, config, newRunner()).UnitContents(); err == nil {
		t.Fatal("expected a refusal for an executable path containing a newline")
	}
}

func TestLaunchdEscapesXMLInThePath(t *testing.T) {
	config := darwinConfig("/Users/ada")
	config.Executable = "/Users/ada/a&b/<agent-overflow>"
	got := mustContents(t, mustNew(t, config, newRunner()))
	if !strings.Contains(got, "<string>/Users/ada/a&amp;b/&lt;agent-overflow&gt;</string>") {
		t.Errorf("the path was not XML-escaped:\n%s", got)
	}
	if strings.Contains(got, "<string>/Users/ada/a&b/<agent-overflow></string>") {
		t.Errorf("the raw path leaked into the plist:\n%s", got)
	}
}

func TestSystemdInstallWritesThenTellsTheManager(t *testing.T) {
	home := t.TempDir()
	runner := newRunner()
	manager := mustNew(t, linuxConfig(home), runner)

	if err := manager.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	body, err := os.ReadFile(manager.UnitPath())
	if err != nil {
		t.Fatalf("read the written unit: %v", err)
	}
	if !strings.Contains(string(body), "ExecStart=/home/ada/.local/bin/agent-overflow supervise") {
		t.Errorf("the written unit does not start the backend:\n%s", body)
	}

	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now agent-overflow.service",
	}
	assertCalls(t, runner, want)
}

// daemon-reload before enable, not after: systemd enables the unit it has
// read, so the reverse order enables a stale one (or nothing at all on a
// first install).
func TestSystemdInstallReloadsBeforeEnabling(t *testing.T) {
	runner := newRunner()
	manager := mustNew(t, linuxConfig(t.TempDir()), runner)
	if err := manager.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(runner.calls) < 2 || !strings.Contains(runner.calls[0], "daemon-reload") {
		t.Errorf("daemon-reload is not the first command: %v", runner.calls)
	}
}

// A unit file that was written but could not be enabled must leave the reason
// visible: systemd's own words, not a paraphrase.
func TestSystemdInstallReportsWhatTheManagerSaid(t *testing.T) {
	runner := newRunner()
	runner.answers["systemctl --user daemon-reload"] = answer{
		output: "Failed to connect to bus: No medium found",
		code:   1,
	}
	manager := mustNew(t, linuxConfig(t.TempDir()), runner)

	err := manager.Install(context.Background())
	if err == nil {
		t.Fatal("expected an install failure")
	}
	if !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("the manager's own message is missing from %q", err)
	}
}

func TestSystemdUninstallRemovesTheUnitEvenWhenNothingWasLoaded(t *testing.T) {
	home := t.TempDir()
	runner := newRunner()
	// A unit systemd never loaded: disable answers non-zero, and uninstall
	// still has to remove the file.
	runner.answers["systemctl --user disable --now agent-overflow.service"] = answer{
		output: "Failed to disable unit: Unit file agent-overflow.service does not exist.",
		code:   1,
	}
	manager := mustNew(t, linuxConfig(home), runner)
	if err := manager.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(manager.UnitPath()); !os.IsNotExist(err) {
		t.Errorf("the unit file survived uninstall: %v", err)
	}
}

// Uninstalling something that was never installed is the state uninstall wants
// to reach, so it succeeds rather than complaining.
func TestSystemdUninstallIsIdempotent(t *testing.T) {
	manager := mustNew(t, linuxConfig(t.TempDir()), newRunner())
	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall on a clean machine: %v", err)
	}
}

func TestSystemdStatusReadsTheManager(t *testing.T) {
	cases := []struct {
		name              string
		enabled, active   string
		enabledC, activeC int
		wantEnabled       bool
		wantRunning       bool
		wantDetail        string
	}{
		{name: "up", enabled: "enabled", active: "active", wantEnabled: true, wantRunning: true, wantDetail: "active"},
		{name: "installed but stopped", enabled: "enabled", active: "inactive", activeC: 3, wantEnabled: true, wantDetail: "inactive"},
		{name: "crashed", enabled: "enabled", active: "failed", activeC: 3, wantEnabled: true, wantDetail: "failed"},
		{name: "unknown to systemd", enabled: "not-found", enabledC: 1, active: "inactive", activeC: 3, wantDetail: "inactive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := newRunner()
			runner.answers["systemctl --user is-enabled agent-overflow.service"] = answer{tc.enabled, tc.enabledC}
			runner.answers["systemctl --user is-active agent-overflow.service"] = answer{tc.active, tc.activeC}
			manager := mustNew(t, linuxConfig(t.TempDir()), runner)

			status, err := manager.Status(context.Background())
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if status.Enabled != tc.wantEnabled || status.Running != tc.wantRunning || status.Detail != tc.wantDetail {
				t.Errorf("Status = {enabled:%v running:%v detail:%q}, want {%v %v %q}",
					status.Enabled, status.Running, status.Detail,
					tc.wantEnabled, tc.wantRunning, tc.wantDetail)
			}
			if status.Installed {
				t.Error("Installed is true with no unit file on disk")
			}
		})
	}
}

func TestSystemdStatusSeesTheUnitFile(t *testing.T) {
	runner := newRunner()
	manager := mustNew(t, linuxConfig(t.TempDir()), runner)
	if err := manager.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Installed {
		t.Errorf("Installed is false with a unit at %s", manager.UnitPath())
	}
}

// systemd reads user units from $XDG_CONFIG_HOME/systemd/user. A host that
// sets it and a unit written to ~/.config would never meet.
func TestSystemdHonorsTheConfigHome(t *testing.T) {
	config := linuxConfig("/home/ada")
	config.ConfigHome = "/home/ada/.local/etc"
	manager := mustNew(t, config, newRunner())
	if want := "/home/ada/.local/etc/systemd/user/agent-overflow.service"; manager.UnitPath() != want {
		t.Errorf("UnitPath = %q, want %q", manager.UnitPath(), want)
	}
}

// bootstrap refuses a label launchd already holds, so a reinstall that skipped
// bootout would leave the OLD plist running while the new file sat on disk.
func TestLaunchdInstallBootsOutFirst(t *testing.T) {
	home := t.TempDir()
	runner := newRunner()
	runner.answers["launchctl bootout gui/501/com.agentoverflow.serve"] = answer{
		output: "Boot-out failed: 3: No such process",
		code:   3,
	}
	config := darwinConfig(home)
	manager := mustNew(t, config, runner)

	if err := manager.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	assertCalls(t, runner, []string{
		"launchctl bootout gui/501/com.agentoverflow.serve",
		"launchctl bootstrap gui/501 " + manager.UnitPath(),
		"launchctl enable gui/501/com.agentoverflow.serve",
	})
	if _, err := os.Stat(filepath.Join(home, "Library", "Logs")); err != nil {
		t.Errorf("the log directory was not created: %v", err)
	}
}

func TestLaunchdUninstallBootsOutThenRemoves(t *testing.T) {
	runner := newRunner()
	manager := mustNew(t, darwinConfig(t.TempDir()), runner)
	if err := manager.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	runner.calls = nil

	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	assertCalls(t, runner, []string{"launchctl bootout gui/501/com.agentoverflow.serve"})
	if _, err := os.Stat(manager.UnitPath()); !os.IsNotExist(err) {
		t.Errorf("the plist survived uninstall: %v", err)
	}
}

func TestLaunchdStatusReadsTheManager(t *testing.T) {
	cases := []struct {
		name        string
		output      string
		code        int
		wantEnabled bool
		wantRunning bool
		wantDetail  string
	}{
		{name: "not loaded", code: 113, wantDetail: "not loaded"},
		{
			name:        "running",
			output:      "com.agentoverflow.serve = {\n\tactive count = 1\n\tstate = running\n}",
			wantEnabled: true, wantRunning: true, wantDetail: "running",
		},
		{
			name:        "waiting",
			output:      "com.agentoverflow.serve = {\n\tstate = waiting\n}",
			wantEnabled: true, wantDetail: "waiting",
		},
		{
			name:        "a state this package has never seen",
			output:      "com.agentoverflow.serve = {\n\tstate = spawn scheduled\n}",
			wantEnabled: true, wantDetail: "spawn scheduled",
		},
		{
			name:        "loaded with no state line",
			output:      "com.agentoverflow.serve = {\n\tactive count = 0\n}",
			wantEnabled: true, wantDetail: "loaded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := newRunner()
			runner.answers["launchctl print gui/501/com.agentoverflow.serve"] = answer{tc.output, tc.code}
			manager := mustNew(t, darwinConfig(t.TempDir()), runner)

			status, err := manager.Status(context.Background())
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if status.Enabled != tc.wantEnabled || status.Running != tc.wantRunning || status.Detail != tc.wantDetail {
				t.Errorf("Status = {enabled:%v running:%v detail:%q}, want {%v %v %q}",
					status.Enabled, status.Running, status.Detail,
					tc.wantEnabled, tc.wantRunning, tc.wantDetail)
			}
		})
	}
}

// A manager binary that is not on the machine at all is a different failure
// from one that answered non-zero, and must not read as "the service is fine".
func TestARunnerThatCannotRunTheCommandIsAnError(t *testing.T) {
	runner := newRunner()
	runner.fail["systemctl --user is-enabled agent-overflow.service"] = errors.New("exec: \"systemctl\": not found")
	manager := mustNew(t, linuxConfig(t.TempDir()), runner)

	if _, err := manager.Status(context.Background()); err == nil {
		t.Fatal("expected an error when systemctl cannot be run")
	}
}

func TestNewRefusesAHostWithNoManagerItDrives(t *testing.T) {
	cases := []struct {
		goos     string
		mentions string
	}{
		{goos: "windows", mentions: "WSL"},
		{goos: "openbsd", mentions: "openbsd"},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			_, err := New(Config{GOOS: tc.goos, Executable: "/opt/agent-overflow", HomeDir: "/home/ada"}, newRunner())
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("New(%s) error = %v, want ErrUnsupported", tc.goos, err)
			}
			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("the refusal does not mention %q: %v", tc.mentions, err)
			}
		})
	}
}

// Every path in a unit file is read by a process with none of this shell's
// context, so a relative one is refused at construction rather than written
// into a service that silently never starts.
func TestNewRefusesInputsThatWouldProduceABrokenUnit(t *testing.T) {
	cases := []struct {
		name   string
		config Config
	}{
		{name: "a relative executable", config: Config{GOOS: "linux", Executable: "agent-overflow", HomeDir: "/home/ada"}},
		{name: "a relative home", config: Config{GOOS: "linux", Executable: "/opt/agent-overflow", HomeDir: "ada"}},
		{name: "a relative config home", config: Config{GOOS: "linux", Executable: "/opt/agent-overflow", HomeDir: "/home/ada", ConfigHome: "etc"}},
		{name: "a listen address with a space", config: Config{GOOS: "linux", Executable: "/opt/agent-overflow", HomeDir: "/home/ada", Listen: "0.0.0.0 7777"}},
		{name: "no goos", config: Config{Executable: "/opt/agent-overflow", HomeDir: "/home/ada"}},
		{name: "darwin with no uid", config: Config{GOOS: "darwin", Executable: "/opt/agent-overflow", HomeDir: "/Users/ada"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.config, newRunner()); err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
}

// The injected Runner is the structural guarantee that `go test` cannot enable
// a service on the developer's machine. A nil one must not fall back to
// anything real.
func TestNewRequiresARunner(t *testing.T) {
	if _, err := New(linuxConfig("/home/ada"), nil); err == nil {
		t.Fatal("New accepted a nil Runner")
	}
}

// Lingering is documented, never run: enabling it changes how the user's
// session behaves for everything on the machine, and that is the operator's
// call to make.
func TestInstallNeverEnablesLingeringItself(t *testing.T) {
	runner := newRunner()
	manager := mustNew(t, linuxConfig(t.TempDir()), runner)
	if err := manager.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "loginctl") || strings.Contains(call, "linger") {
			t.Errorf("install ran %q; lingering is the operator's decision", call)
		}
	}
	var notes string
	for _, note := range manager.Notes() {
		notes += note + "\n"
	}
	if !strings.Contains(notes, "loginctl enable-linger") {
		t.Errorf("the notes do not tell the operator how to enable lingering:\n%s", notes)
	}
}

func assertCalls(t *testing.T, runner *fakeRunner, want []string) {
	t.Helper()
	if len(runner.calls) != len(want) {
		t.Fatalf("commands = %v, want %v", runner.calls, want)
	}
	for i, call := range runner.calls {
		if call != want[i] {
			t.Errorf("command %d = %q, want %q", i, call, want[i])
		}
	}
}

func TestStopDoesNotTreatSignalDeliveryAsProcessExit(t *testing.T) {
	runner := newRunner()
	cfg := linuxConfig(t.TempDir())
	cfg.GOOS, cfg.UID = "darwin", "501"
	target := "gui/501/" + LaunchdLabel
	runner.answers["launchctl print "+target] = answer{output: "state = exiting\n pid = 42", code: 0}
	manager, err := New(cfg, runner)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := manager.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop with a surviving pid = %v, want cancellation rather than success", err)
	}
}

func TestStopReportsManagerRefusals(t *testing.T) {
	runner := newRunner()
	manager, err := New(linuxConfig(t.TempDir()), runner)
	if err != nil {
		t.Fatal(err)
	}
	runner.answers["systemctl --user stop "+ServiceName+".service"] = answer{output: "access denied", code: 1}
	if err := manager.Stop(t.Context()); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("stop = %v, want manager refusal", err)
	}
}
