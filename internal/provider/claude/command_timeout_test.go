package claude

import (
	"testing"

	"agent-overflow/internal/provider"
)

// The setting travels SessionOptions -> Config.CommandTimeoutHours ->
// BASH_MAX_TIMEOUT_MS in ms, outranks a value inherited from the host, and
// yields to a value in the caller's env (the provider's custom
// environment). Serial: it sets the process environment.
func TestCommandTimeoutReachesTheSpawnEnvironment(t *testing.T) {
	t.Setenv(BashMaxTimeoutEnv, "60000")

	cfg := ConfigFromOptions(provider.SessionOptions{
		Provider:                     "claude",
		Model:                        "claude-opus-4-7",
		ClaudeMaxCommandTimeoutHours: 5,
	})
	if cfg.CommandTimeoutHours != 5 {
		t.Fatalf("CommandTimeoutHours = %d, want 5", cfg.CommandTimeoutHours)
	}
	env := claudeChildEnv(cfg)
	if value, found := envLookup(env, BashMaxTimeoutEnv); !found || value != "18000000" {
		t.Fatalf("%s = %q (present=%v), want 18000000 over the inherited value", BashMaxTimeoutEnv, value, found)
	}

	cfg.Env = provider.EnvInput{Custom: map[string]string{BashMaxTimeoutEnv: "99999999"}}
	env = claudeChildEnv(cfg)
	if value, _ := envLookup(env, BashMaxTimeoutEnv); value != "99999999" {
		t.Fatalf("%s = %q, want the custom environment's value", BashMaxTimeoutEnv, value)
	}
}

func TestWithCommandTimeoutEnv(t *testing.T) {
	t.Parallel()
	if got := WithCommandTimeoutEnv(provider.EnvInput{}, 0); got.Defaults != nil {
		t.Fatalf("zero hours = %+v, want nothing exported", got)
	}
	got := WithCommandTimeoutEnv(provider.EnvInput{}, 168)
	if got.Defaults[provider.ClaudeBashMaxTimeoutEnv] != "604800000" {
		t.Fatalf("168 h = %q, want 604800000", got.Defaults[provider.ClaudeBashMaxTimeoutEnv])
	}
	input := provider.EnvInput{Custom: map[string]string{"FOO": "bar"}}
	got = WithCommandTimeoutEnv(input, 2)
	if got.Defaults[provider.ClaudeBashMaxTimeoutEnv] != "7200000" || got.Custom["FOO"] != "bar" {
		t.Fatalf("2 h = %+v", got)
	}
	if input.Defaults != nil {
		t.Fatal("WithCommandTimeoutEnv wrote into the caller's input")
	}
}

// claudeChildEnv is the environment a headless Claude child gets from cfg:
// the launch's clears, then the session's pins and defaults.
func claudeChildEnv(cfg Config) []string {
	return provider.BuildEnvironment(claudeSpawnEnv(cfg).Clear(launchClears()...))
}
