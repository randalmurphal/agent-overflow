package claude

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// NativeMemoryOff sends `autoMemoryEnabled: false` in the inline settings,
// alone or beside the block's other keys, and states the variable that would
// force memory back on in the inline settings' env, which outranks the user's
// settings files and the inherited environment. Without it neither the key
// nor the variable is sent, so the user's own settings decide.
func TestNativeMemoryOffRidesTheInlineSettings(t *testing.T) {
	for name, opts := range map[string]provider.SessionOptions{
		"alone":       {Provider: "claude", Model: "claude-opus-4-7"},
		"beside keys": {Provider: "claude", Model: "claude-opus-4-7", FastMode: true},
	} {
		cfg := ConfigFromOptions(opts)
		cfg.NativeMemoryOff = true
		settings := settingsArg(t, buildArgs(cfg, "", ""))
		if got, ok := settings["autoMemoryEnabled"]; !ok || got != false {
			t.Errorf("%s: autoMemoryEnabled = %v, %v", name, got, ok)
		}
		if env, _ := settings["env"].(map[string]any); env[AutoMemoryOffEnv] != "1" {
			t.Errorf("%s: inline env = %v", name, env)
		}
		cfg.NativeMemoryOff = false
		joined := strings.Join(buildArgs(cfg, "", ""), " ")
		if strings.Contains(joined, "autoMemoryEnabled") || strings.Contains(joined, AutoMemoryOffEnv) {
			t.Errorf("%s: sent autoMemoryEnabled or %s without NativeMemoryOff: %s", name, AutoMemoryOffEnv, joined)
		}
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
