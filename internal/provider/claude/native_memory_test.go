package claude

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// NativeMemoryOff sends `autoMemoryEnabled: false` in the inline settings,
// alone or beside the block's other keys; without it the key is absent, so
// the user's own settings decide.
func TestNativeMemoryOffRidesTheInlineSettings(t *testing.T) {
	for name, opts := range map[string]provider.SessionOptions{
		"alone":       {Provider: "claude", Model: "claude-opus-4-7"},
		"beside keys": {Provider: "claude", Model: "claude-opus-4-7", FastMode: true},
	} {
		cfg := ConfigFromOptions(opts)
		cfg.NativeMemoryOff = true
		if got, ok := settingsArg(t, buildArgs(cfg, "", ""))["autoMemoryEnabled"]; !ok || got != false {
			t.Errorf("%s: autoMemoryEnabled = %v, %v", name, got, ok)
		}
		cfg.NativeMemoryOff = false
		if strings.Contains(strings.Join(buildArgs(cfg, "", ""), " "), "autoMemoryEnabled") {
			t.Errorf("%s: sent autoMemoryEnabled without NativeMemoryOff", name)
		}
	}
}

// NativeMemoryOff also states the variable that would force memory back on
// in the inline settings' env, which outranks the user's settings files and
// the inherited environment; without it the block names no such variable.
func TestNativeMemoryOffStatesTheVariable(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{Provider: "claude", Model: "claude-opus-4-7"})
	cfg.NativeMemoryOff = true
	env, _ := settingsArg(t, buildArgs(cfg, "", ""))["env"].(map[string]any)
	if env[AutoMemoryOffEnv] != "1" {
		t.Fatalf("inline env = %v", env)
	}
	cfg.NativeMemoryOff = false
	if strings.Contains(strings.Join(buildArgs(cfg, "", ""), " "), AutoMemoryOffEnv) {
		t.Fatal("stated the variable without NativeMemoryOff")
	}
}

// AccessDirs ride the inline settings' permissions, never `--add-dir`,
// which would make each a root Claude Code loads skills and CLAUDE.md from.
func TestAccessDirsRideTheInlinePermissions(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{Provider: "claude", Model: "claude-opus-4-7"})
	cfg.AdditionalDirs = []string{"/attachments"}
	cfg.AccessDirs = []string{"/memory/user", "/memory/projects/app"}
	args := buildArgs(cfg, "", "")
	permissions, ok := settingsArg(t, args)["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("no permissions in the inline settings: %q", args)
	}
	if got, _ := permissions["additionalDirectories"].([]any); len(got) != 2 || got[0] != "/memory/user" || got[1] != "/memory/projects/app" {
		t.Fatalf("additionalDirectories = %v", permissions["additionalDirectories"])
	}
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "--add-dir\x00/attachments") || strings.Contains(joined, "--add-dir\x00/memory") {
		t.Fatalf("--add-dir carries the wrong folders: %q", args)
	}
	cfg.AccessDirs = nil
	if _, ok := settingsArg(t, buildArgs(cfg, "", ""))["permissions"]; ok {
		t.Fatal("permissions sent without AccessDirs")
	}
}
