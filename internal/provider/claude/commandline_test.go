package claude

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
)

// withTestRetention answers for the user's home with no period when cfg, in
// a test that does not exercise retention, gives no answer.
func withTestRetention(cfg Config) Config {
	if cfg.Retention == (Retention{}) {
		cfg.Retention = UserHomeRetention(0)
	}
	return cfg
}

// buildArgs is the session argv for tests that do not exercise retention.
func buildArgs(cfg Config, systemPromptPath, appendSystemPromptPath string) []string {
	line, err := sessionCommandLine(withTestRetention(cfg), systemPromptPath, appendSystemPromptPath)
	if err != nil {
		panic(err)
	}
	return line.Args()
}

func TestNewCommandLineRefusesAnUnansweredRetention(t *testing.T) {
	if _, err := NewCommandLine(Retention{}, "", "doctor"); !errors.Is(err, errNoRetention) {
		t.Fatalf("NewCommandLine(zero) error = %v, want errNoRetention", err)
	}
	if _, err := NewCommandLine(IsolatedHomeRetention(" "), ""); err == nil {
		t.Fatal("an isolated home with no directory built a command line")
	}
}

// Every Claude launch path refuses to start without a Retention, before it
// starts anything.
func TestClaudeLaunchesRefuseAnUnansweredRetention(t *testing.T) {
	ctx := context.Background()
	if _, err := NewSession(ctx, "thread", Config{Reaper: procutiltest.Reaper(), Binary: "/nonexistent/claude", WorkDir: t.TempDir()}, func(provider.ProviderEvent) {}); !errors.Is(err, errNoRetention) {
		t.Errorf("NewSession error = %v, want errNoRetention", err)
	}
	if _, err := ProbeAccount(ctx, ProbeConfig{Reaper: procutiltest.Reaper(), Binary: "/nonexistent/claude", WorkDir: t.TempDir()}); !errors.Is(err, errNoRetention) {
		t.Errorf("ProbeAccount error = %v, want errNoRetention", err)
	}
}

func TestNewCommandLinePutsTheSettingsBlockFirst(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention Retention
		settings  string
		want      []string
	}{
		{"period alone", UserHomeRetention(90), "", []string{"--settings", `{"cleanupPeriodDays":90}`, "doctor"}},
		{"no period, no block", UserHomeRetention(0), "", []string{"doctor"}},
		{"negative period sends none", UserHomeRetention(-3), "", []string{"doctor"}},
		{"after the launch's own keys", UserHomeRetention(90), `{"fastMode":true}`, []string{"--settings", `{"fastMode":true,"cleanupPeriodDays":90}`, "doctor"}},
		{"own keys alone", UserHomeRetention(0), `{"fastMode":true}`, []string{"--settings", `{"fastMode":true}`, "doctor"}},
		{"an empty object sends none", UserHomeRetention(0), `{}`, []string{"doctor"}},
		{"no sweep carries none", NoSweepRetention(), "", []string{"doctor"}},
		{"an isolated home carries none", IsolatedHomeRetention("/tmp/x"), "", []string{"doctor"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, err := NewCommandLine(tc.retention, tc.settings, "doctor")
			if err != nil {
				t.Fatalf("NewCommandLine: %v", err)
			}
			if got := line.Args(); !slices.Equal(got, tc.want) {
				t.Fatalf("Args = %q, want %q", got, tc.want)
			}
		})
	}
}

// Only the Retention sets the period: a block that names it is refused, as
// is one that is not a JSON object.
func TestNewCommandLineRefusesASettingsBlockItDoesNotOwn(t *testing.T) {
	for _, settings := range []string{`{"cleanupPeriodDays":7}`, `[1]`, `not json`} {
		if _, err := NewCommandLine(UserHomeRetention(90), settings); err == nil {
			t.Errorf("NewCommandLine(%q) built a command line", settings)
		}
	}
}

// Every launch clears the config-home, cross-session and task-list
// variables; an isolated home pins its directory over the cleared one.
func TestNewCommandLineEnvironment(t *testing.T) {
	want := []provider.ReservedEnv{
		provider.ClaudeConfigDirEnv,
		provider.ClaudeSecureStorageConfigDirEnv,
		provider.ClaudeCrossSessionGateEnv,
		provider.ClaudePeerSessionNameEnv,
		provider.ClaudeTaskListIDEnv,
	}
	custom := map[string]string{"A": "1"}
	for _, retention := range []Retention{UserHomeRetention(90), NoSweepRetention(), IsolatedHomeRetention("/tmp/x")} {
		line, err := NewCommandLine(retention, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := line.Env(provider.EnvInput{Custom: custom}); !slices.Equal(got.Clears, want) || got.Custom["A"] != "1" {
			t.Errorf("%+v Env = %+v, want the custom environment with clears %v", retention, got, want)
		}
	}
	user, _ := NewCommandLine(UserHomeRetention(0), "")
	if got := user.Env(provider.EnvInput{}); got.Pins != nil {
		t.Errorf("user-home Env pins %v, want none", got.Pins)
	}
	isolated, _ := NewCommandLine(IsolatedHomeRetention("/tmp/x"), "")
	if got := isolated.Env(provider.EnvInput{}); len(got.Pins) != 1 || got.Pins[provider.ClaudeConfigDirEnv] != "/tmp/x" {
		t.Errorf("isolated Env pins %v, want CLAUDE_CONFIG_DIR=/tmp/x", got.Pins)
	}
	if strings.Contains(strings.Join(isolated.Args(), " "), "--settings") {
		t.Error("an isolated home sent a settings block")
	}
}
