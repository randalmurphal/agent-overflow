package providerupdate

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

func TestClaudeLatestFollowsTheDoctorChannel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		channel, latest string
	}{
		{"latest", "2.1.325"},
		{"stable", "2.1.311"},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.setInstalled(ProviderClaude, "2.1.200")
			r.runner.on("doctor", doctorSays(tc.channel))
			claude, _ := r.check()
			if claude.LatestVersion != tc.latest || !claude.Available || claude.CheckError != "" {
				t.Fatalf("claude = %+v, want latest %s available", claude, tc.latest)
			}
			if claude.InstalledVersion != "2.1.200" || claude.CheckedAt != 1_700_000_000_000 {
				t.Errorf("claude = %+v", claude)
			}
			doctor := r.runner.lastOf("doctor")
			want := Command{Binary: "/opt/claude", Args: []string{"doctor"}, Env: provider.EnvInput{Custom: map[string]string{"PROXY_FOR": ProviderClaude}}, Dir: "/home/u"}
			if doctor.Binary != want.Binary || !slices.Equal(doctor.Args, want.Args) || !maps.Equal(doctor.Env.Custom, want.Env.Custom) || doctor.Dir != want.Dir {
				t.Errorf("doctor ran %+v, want %+v", doctor, want)
			}
			if got := r.hosts.count(ClaudeChannelURL(tc.channel)); got != 1 {
				t.Errorf("channel %s fetched %d times", tc.channel, got)
			}
		})
	}
}

func TestCodexLatestReadsTheNpmManifest(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	_, codex := r.check()
	if codex.LatestVersion != "0.169.0" || !codex.Available || codex.InstalledVersion != "0.160.0" {
		t.Fatalf("codex = %+v", codex)
	}
	if r.hosts.count(CodexNPMLatestURL) != 1 {
		t.Fatalf("requests = %v", r.hosts.requested())
	}
	r.hosts.mu.Lock()
	agents := slices.Clone(r.hosts.agents)
	r.hosts.mu.Unlock()
	for _, agent := range agents {
		if agent != userAgent {
			t.Errorf("request sent User-Agent %q", agent)
		}
	}
}

// A Codex the installer unpacked reads the latest GitHub release, and its
// run installs exactly that release: the installer installs CODEX_RELEASE,
// not whatever is latest when the run starts.
func TestAStandaloneCodexInstallsTheReleaseItsCheckOffered(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.setBinary(ProviderCodex, standaloneCodex)
	userEnv := map[string]string{"PROXY_FOR": ProviderCodex}
	r.mu.Lock()
	r.env = map[string]map[string]string{ProviderCodex: userEnv}
	r.mu.Unlock()
	_, codex := r.check()
	if codex.LatestVersion != "0.168.0" || !codex.Available || codex.CheckError != "" {
		t.Fatalf("codex = %+v, want the latest release 0.168.0 offered", codex)
	}
	if r.hosts.count(CodexLatestReleaseURL) != 1 || r.hosts.count(CodexNPMLatestURL) != 0 {
		t.Fatalf("requests = %v, want only the latest release", r.hosts.requested())
	}

	// The latest release moves on before the run; the run installs what
	// its own check read.
	r.hosts.text(CodexLatestReleaseURL, http.StatusOK, `{"tag_name":"rust-v0.169.0"}`)
	r.runner.on("update", func(context.Context) (Output, error) {
		r.setInstalled(ProviderCodex, "0.169.0")
		return Output{}, nil
	})
	mark := r.mark()
	if err := r.svc.Run(ProviderCodex); err != nil {
		t.Fatal(err)
	}
	if status := r.waitRunSettled(mark, ProviderCodex); status.Run.Phase != PhaseSucceeded {
		t.Fatalf("run = %+v", status.Run)
	}
	update := r.runner.lastOf("update")
	want := provider.EnvInput{
		Custom: map[string]string{"PROXY_FOR": ProviderCodex},
		Pins:   map[provider.ReservedEnv]string{provider.CodexReleaseEnv: "0.169.0"},
	}
	if update.Binary != standaloneCodex.Path || !slices.Equal(update.Args, []string{"update"}) ||
		!maps.Equal(update.Env.Custom, want.Custom) || !maps.Equal(update.Env.Pins, want.Pins) {
		t.Fatalf("ran %+v, want %s update with %+v", update, standaloneCodex.Path, want)
	}
	if len(userEnv) != 1 {
		t.Errorf("the pin was written into the user's environment: %v", userEnv)
	}
}

func TestALatestReleaseThatIsNotACodexTagFailsTheCheck(t *testing.T) {
	t.Parallel()
	for _, tag := range []string{"0.168.0", "v0.168.0", "rust-vnext", ""} {
		r := newRig(t)
		r.setBinary(ProviderCodex, standaloneCodex)
		r.hosts.text(CodexLatestReleaseURL, http.StatusOK, `{"tag_name":"`+tag+`"}`)
		_, codex := r.check()
		if codex.Available || codex.CheckError == "" || codex.LatestVersion != "" {
			t.Errorf("tag %q: codex = %+v, want a failed check", tag, codex)
		}
	}
}

// A Codex no package manager or installer owns has nothing to run, so the
// check says so and offers nothing, without asking any release host.
func TestACodexWithNoUpdaterFailsItsCheck(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.setBinary(ProviderCodex, Binary{Path: "/opt/codex/bin/codex", ResolvedPath: "/opt/codex/bin/codex"})
	_, codex := r.check()
	if codex.Available || codex.InstalledVersion != "0.160.0" || !strings.Contains(codex.CheckError, "not installed with npm, Homebrew or the Codex installer") {
		t.Fatalf("codex = %+v, want its installed version and no updater reported", codex)
	}
	for _, url := range r.hosts.requested() {
		if url != ClaudeChannelURL("latest") {
			t.Errorf("a Codex with no updater requested %s", url)
		}
	}
	if err := r.svc.Run(ProviderCodex); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("Run = %v, want ErrNoUpdate", err)
	}
}

func TestParseDoctorChannel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, output, want, err string
	}{
		{"plain", "Running: native (2.1.321)\nAuto-update channel: latest\n", "latest", ""},
		{"ansi", "\x1b[1mDiagnostics\x1b[0m\n \x1b[2m└\x1b[0m Auto-update channel: \x1b[32mstable\x1b[0m\r\n", "stable", ""},
		{"missing", "Running: native (2.1.321)\nAuto-updates: disabled (set by env: DISABLE_AUTOUPDATER)\n", "", `no "Auto-update channel:" line`},
		{"unknown value", "Auto-update channel: ../../evil\n", "", `"../../evil", which is neither latest nor stable`},
		{"empty value", "Auto-update channel:\n", "", `""`},
	} {
		got, err := parseDoctorChannel(tc.output)
		if tc.err == "" {
			if err != nil || got != tc.want {
				t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: err = %v, want it to contain %s", tc.name, err, tc.err)
		}
	}
}

func TestDoctorFailuresReachCheckError(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.runner.on("doctor", func(context.Context) (Output, error) {
		return Output{Text: "boom", ExitCode: 2}, nil
	})
	claude, codex := r.check()
	if claude.Available || claude.LatestVersion != "" || !strings.Contains(claude.CheckError, "status 2") ||
		!strings.Contains(claude.CheckError, "Auto-update channel:") {
		t.Fatalf("claude = %+v", claude)
	}
	if !codex.Available || codex.CheckError != "" {
		t.Errorf("a claude failure reached codex: %+v", codex)
	}
	for _, url := range r.hosts.requested() {
		if strings.HasPrefix(url, claudeChannelBase) {
			t.Errorf("fetched %s without a channel", url)
		}
	}

	r.runner.on("doctor", func(context.Context) (Output, error) { return Output{}, errNoAnswer })
	claude, _ = r.check()
	if !strings.Contains(claude.CheckError, "could not run claude doctor") {
		t.Errorf("runner error = %q", claude.CheckError)
	}
}

func TestADowngradeChannelIsNotAnUpdate(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.setInstalled(ProviderClaude, "2.1.321")
	r.runner.on("doctor", doctorSays("stable"))
	claude, _ := r.check()
	if claude.Available || claude.LatestVersion != "2.1.311" || claude.CheckError != "" {
		t.Fatalf("claude = %+v, want stable's older 2.1.311 known but not offered", claude)
	}
	err := r.svc.Run(ProviderClaude)
	if err == nil || !strings.Contains(err.Error(), ErrNoUpdate.Error()) {
		t.Fatalf("Run = %v, want ErrNoUpdate", err)
	}
	if r.runner.ran("update") != 0 {
		t.Fatal("the downgrade ran")
	}
}

func TestAFailedCheckClearsTheLatestVersion(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if claude, _ := r.check(); !claude.Available {
		t.Fatalf("first check = %+v", claude)
	}
	r.hosts.text(ClaudeChannelURL("latest"), http.StatusBadGateway, "down")
	r.hosts.text(CodexNPMLatestURL, http.StatusOK, `{"version":"not a version"}`)
	claude, codex := r.check()
	if claude.LatestVersion != "" || claude.Available || !strings.Contains(claude.CheckError, "502") {
		t.Errorf("claude = %+v, want the old latest cleared and the status reported", claude)
	}
	if codex.LatestVersion != "" || codex.Available || !strings.Contains(codex.CheckError, "not a version") {
		t.Errorf("codex = %+v", codex)
	}
	if err := r.svc.Run(ProviderClaude); err == nil {
		t.Error("Run started on a failed check")
	}
}

func TestUnreadableInstalledVersionIsACheckError(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.setInstalled(ProviderCodex, "")
	r.setInstalled(ProviderClaude, "nonsense")
	claude, codex := r.check()
	if codex.Available || codex.CheckError != "could not read the installed codex version" || r.hosts.count(CodexNPMLatestURL) != 0 {
		t.Errorf("codex = %+v", codex)
	}
	if claude.Available || !strings.Contains(claude.CheckError, "nonsense") {
		t.Errorf("claude = %+v", claude)
	}
}

func TestResponsesOverTheLimitFail(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.hosts.text(ClaudeChannelURL("latest"), http.StatusOK, "2.1.325"+strings.Repeat(" ", versionBodyLimit))
	r.hosts.text(CodexNPMLatestURL, http.StatusOK, `{"version":"0.169.0","pad":"`+strings.Repeat("x", manifestBodyLimit)+`"}`)
	claude, codex := r.check()
	for _, status := range []Status{claude, codex} {
		if status.Available || !strings.Contains(status.CheckError, "more than") {
			t.Errorf("%s = %+v, want the oversized body refused", status.Provider, status)
		}
	}
	r.hosts.text(ClaudeChannelURL("latest"), http.StatusOK, "2.1.325"+strings.Repeat(" ", versionBodyLimit-len("2.1.325")))
	if claude, _ := r.check(); !claude.Available {
		t.Errorf("a body at the limit was refused: %+v", claude)
	}
}

func TestBrewCaskNeedsTheExactCaskDirectory(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"/opt/homebrew/Caskroom/claude-code/2.1.3/claude":        "claude-code",
		"/opt/homebrew/Caskroom/claude-code@latest/2.1.3/claude": "claude-code@latest",
		"/opt/homebrew/Caskroom/claude-code-nightly/1/claude":    "",
		"/home/u/Caskroom-ish/claude-code/claude":                "",
		"": "",
	} {
		if got := brewCaskOf(ProviderClaude, path); got != want {
			t.Errorf("brewInstall(%q) = %q, want %q", path, got, want)
		}
	}
	if got := brewCaskOf(ProviderCodex, "/opt/homebrew/Caskroom/claude-code/1/claude"); got != "" {
		t.Errorf("codex matched claude's cask: %q", got)
	}
}

func brewCaskOf(providerName, path string) string {
	cask, _ := brewInstall(providerName, path)
	return cask
}

// A Homebrew install reads its cask's version and is upgraded by the Homebrew
// whose Caskroom holds it: `claude update` on one only prints advice and
// exits 0.
func TestABrewInstallIsUpgradedByItsHomebrew(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		provider, resolved, brew, cask string
	}{
		{ProviderClaude, "/opt/homebrew/Caskroom/claude-code/2.1.300/claude", "/opt/homebrew/bin/brew", "claude-code"},
		{ProviderClaude, "/usr/local/Caskroom/claude-code@latest/2.1.300/claude", "/usr/local/bin/brew", "claude-code@latest"},
		{ProviderCodex, "/opt/homebrew/Caskroom/codex/0.160.0/codex-aarch64-apple-darwin", "/opt/homebrew/bin/brew", "codex"},
		{ProviderCodex, "/usr/local/Caskroom/codex/0.160.0/codex-x86_64-apple-darwin", "/usr/local/bin/brew", "codex"},
	} {
		t.Run(tc.resolved, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.setBinary(tc.provider, Binary{Path: "/opt/homebrew/bin/" + tc.provider, ResolvedPath: tc.resolved})
			r.hosts.text(BrewCaskURL(tc.cask), http.StatusOK, `{"version":"9.9.9"}`)
			claude, codex := r.check()
			if status := map[string]Status{ProviderClaude: claude, ProviderCodex: codex}[tc.provider]; status.LatestVersion != "9.9.9" || !status.Available {
				t.Fatalf("%s = %+v, want the cask's 9.9.9", tc.provider, status)
			}
			if tc.provider == ProviderCodex && r.hosts.count(CodexNPMLatestURL) != 0 {
				t.Error("a brew codex read the npm manifest")
			}
			r.runner.on(tc.brew+" upgrade", func(context.Context) (Output, error) {
				r.setInstalled(tc.provider, "9.9.9")
				return Output{}, nil
			})
			mark := r.mark()
			if err := r.svc.Run(tc.provider); err != nil {
				t.Fatal(err)
			}
			if status := r.waitRunSettled(mark, tc.provider); status.Run.Phase != PhaseSucceeded {
				t.Fatalf("run = %+v", status.Run)
			}
			upgrade := r.runner.lastOf("upgrade")
			if upgrade.Binary != tc.brew || !slices.Equal(upgrade.Args, []string{"upgrade", "--cask", tc.cask}) || upgrade.Dir != "/home/u" {
				t.Errorf("ran %+v, want %s upgrade --cask %s", upgrade, tc.brew, tc.cask)
			}
			if r.runner.ran("update") != 0 || tc.provider == ProviderClaude && r.runner.ran("doctor") != 0 {
				t.Error("a brew install ran the provider's own update or doctor")
			}
		})
	}
}

// Anything Homebrew does not own runs the provider's own updater: the
// configured Claude binary, the Codex npm launcher.
func TestANonBrewInstallRunsTheProvidersUpdate(t *testing.T) {
	t.Parallel()
	for _, name := range []string{ProviderClaude, ProviderCodex} {
		r := newRig(t)
		r.hosts.text(CodexNPMLatestURL, http.StatusOK, `{"version":"0.169.0"}`)
		r.check()
		r.runner.on("update", func(context.Context) (Output, error) {
			r.setInstalled(name, map[string]string{ProviderClaude: "2.1.325", ProviderCodex: "0.169.0"}[name])
			return Output{}, nil
		})
		mark := r.mark()
		if err := r.svc.Run(name); err != nil {
			t.Fatal(err)
		}
		if status := r.waitRunSettled(mark, name); status.Run.Phase != PhaseSucceeded {
			t.Fatalf("%s run = %+v", name, status.Run)
		}
		want := map[string]string{ProviderClaude: "/opt/claude", ProviderCodex: npmCodex.Launcher}[name]
		if update := r.runner.lastOf("update"); update.Binary != want || !slices.Equal(update.Args, []string{"update"}) {
			t.Errorf("%s ran %+v, want %s update", name, update, want)
		}
		if r.runner.ran("upgrade") != 0 {
			t.Errorf("%s ran brew", name)
		}
	}
}

// `claude doctor` and `claude update` are Claude launches against the user's
// home: they carry the retention resolved for their work directory ahead of
// the subcommand and clear the inherited config-home variables. Codex and
// brew commands are not Claude launches and carry neither.
func TestClaudeCommandsCarryTheTranscriptRetention(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.mu.Lock()
	r.retention = claude.UserHomeRetention(90)
	r.mu.Unlock()
	r.hosts.text(CodexNPMLatestURL, http.StatusOK, `{"version":"0.169.0"}`)
	r.check()
	r.runner.on("update", func(context.Context) (Output, error) {
		r.setInstalled(ProviderClaude, "2.1.325")
		return Output{}, nil
	})
	mark := r.mark()
	if err := r.svc.Run(ProviderClaude); err != nil {
		t.Fatal(err)
	}
	if status := r.waitRunSettled(mark, ProviderClaude); status.Run.Phase != PhaseSucceeded {
		t.Fatalf("run = %+v", status.Run)
	}
	r.runner.mu.Lock()
	commands := slices.Clone(r.runner.commands)
	r.runner.mu.Unlock()
	var claudeCommands int
	for _, command := range commands {
		block := []string{"--settings", `{"cleanupPeriodDays":90}`}
		if command.Binary != "/opt/claude" {
			if slices.Contains(command.Args, "--settings") || len(command.Env.Clears) != 0 || command.Env.Pins != nil {
				t.Errorf("%s %q carries Claude's launch rules", command.Binary, command.Args)
			}
			continue
		}
		claudeCommands++
		if !slices.Equal(command.Args, append(block, verb(command))) {
			t.Errorf("claude %s args = %q, want the block first", verb(command), command.Args)
		}
		for _, env := range []provider.ReservedEnv{provider.ClaudeConfigDirEnv, provider.ClaudeSecureStorageConfigDirEnv} {
			if !slices.Contains(command.Env.Clears, env) {
				t.Errorf("claude %s keeps an inherited %s", verb(command), env.Name())
			}
		}
		if command.Env.Custom["PROXY_FOR"] != ProviderClaude {
			t.Errorf("claude %s lost its environment: %v", verb(command), command.Env)
		}
	}
	if claudeCommands == 0 || r.runner.ran("update") != 1 {
		t.Errorf("ran %d Claude commands, want doctor and one update: %+v", claudeCommands, commands)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, dir := range r.retentionDirs {
		if dir != "/home/u" {
			t.Errorf("retention resolved for %q, want the work directory", dir)
		}
	}
}

// A Claude command with no retention answer is refused, and the check says so.
func TestAClaudeCommandWithNoRetentionAnswerFailsTheCheck(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.mu.Lock()
	r.retention = claude.Retention{}
	r.mu.Unlock()
	status, _ := r.check()
	if status.CheckError == "" || r.runner.ran("doctor") != 0 {
		t.Fatalf("claude = %+v, doctor ran %d times; want a refused check", status, r.runner.ran("doctor"))
	}
}

// Past the login's refresh deadline a Claude start renews the token and the
// renewal blanks the saved credential, so the check runs no `claude doctor`
// and says a sign-in is needed instead of failing. A brew install asks no
// Claude process and is still checked.
func TestAnExpiredClaudeLoginRunsNoDoctor(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.setInstalled(ProviderClaude, "2.1.200")
	r.check()
	r.mu.Lock()
	r.loginExpired = true
	r.mu.Unlock()
	status, _ := r.check()
	if r.runner.ran("doctor") != 1 {
		t.Fatalf("doctor ran %d times, want only the check before the login expired", r.runner.ran("doctor"))
	}
	if !status.SignInRequired || status.CheckError != "" || status.LatestVersion != "" || status.Available {
		t.Fatalf("claude = %+v, want a sign-in required and nothing offered", status)
	}
	if err := r.svc.Run(ProviderClaude); !errors.Is(err, ErrNoUpdate) {
		t.Errorf("Run = %v, want ErrNoUpdate", err)
	}

	r.mu.Lock()
	r.loginExpired = false
	r.mu.Unlock()
	if status, _ := r.check(); status.SignInRequired || !status.Available {
		t.Errorf("claude after a sign-in = %+v, want the update offered again", status)
	}

	r.mu.Lock()
	r.loginExpired = true
	r.mu.Unlock()
	r.setBinary(ProviderClaude, Binary{Path: "/opt/homebrew/bin/claude", ResolvedPath: "/opt/homebrew/Caskroom/claude-code/2.1.200/claude"})
	r.hosts.text(BrewCaskURL("claude-code"), http.StatusOK, `{"version":"9.9.9"}`)
	if status, _ := r.check(); status.SignInRequired || !status.Available {
		t.Errorf("brew claude = %+v, want it checked", status)
	}
}

// A login that cannot be read fails the check, and nothing is spawned.
func TestAnUnreadableClaudeLoginFailsTheCheck(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.mu.Lock()
	r.loginErr = errors.New("keychain locked")
	r.mu.Unlock()
	status, _ := r.check()
	if r.runner.ran("doctor") != 0 || !strings.Contains(status.CheckError, "keychain locked") || status.SignInRequired {
		t.Fatalf("claude = %+v, doctor ran %d times; want the read failure reported", status, r.runner.ran("doctor"))
	}
}

// `claude doctor` is stopped once it has printed the channel, so one that
// lingers with no terminal does not hold a process until its deadline.
func TestDoctorIsDoneOnceItNamesTheChannel(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.check()
	doctor := r.runner.lastOf("doctor")
	if doctor.Done == nil {
		t.Fatal("doctor ran with no Done")
	}
	for output, want := range map[string]bool{
		"Diagnostics\n  Running: native (2.1.300)\n":   false,
		"  Auto-update channel: lat":                   false,
		"  Auto-update channel: latest\n  Next line\n": true,
		"\x1b[1mAuto-update channel:\x1b[0m stable\n":  true,
	} {
		if got := doctor.Done(output); got != want {
			t.Errorf("Done(%q) = %v, want %v", output, got, want)
		}
	}
	if update := r.runner.lastOf("update"); update.Done != nil {
		t.Error("an update command is stopped early")
	}
}

// A doctor that never names its channel is stopped at its deadline, and
// the check says that rather than a bare context error.
func TestADoctorThatNeverAnswersSaysSo(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.runner.on("doctor", func(context.Context) (Output, error) {
		return Output{Text: "Diagnostics\n", ExitCode: -1}, context.DeadlineExceeded
	})
	status, _ := r.check()
	if want := "claude doctor did not report its auto-update channel within 30s"; !strings.Contains(status.CheckError, want) {
		t.Fatalf("CheckError = %q, want it to contain %q", status.CheckError, want)
	}
}
