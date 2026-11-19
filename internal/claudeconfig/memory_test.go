package claudeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Claude's auto memory is decided as Claude Code decides it: the variable
// when it holds a true or false word, taken from the user's settings `env`,
// else ~/.claude.json's `env`, else the spawn environment; otherwise the
// user's settings, on when they say nothing. A file that cannot be parsed
// is an error naming it.
func TestAutoMemoryFollowsTheVariableThenTheSettings(t *testing.T) {
	t.Parallel()
	storeWith := func(global, settings string) *Store {
		home := t.TempDir()
		if global != "" {
			if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(global), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if settings != "" {
			if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return New(PathForHome(home))
	}
	const off, silent = `{"autoMemoryEnabled": false}`, `{"model": "opus"}`
	for name, tc := range map[string]struct {
		env              string
		set              bool
		global, settings string
		want             bool
	}{
		"no settings":                  {want: true},
		"settings silent":              {settings: silent, want: true},
		"settings off":                 {settings: off, want: false},
		"variable true":                {env: "1", set: true, settings: silent, want: false},
		"variable TRUE":                {env: " TRUE ", set: true, settings: silent, want: false},
		"variable false beats setting": {env: "false", set: true, settings: off, want: true},
		"variable other word ignored":  {env: "maybe", set: true, settings: off, want: false},
		"variable empty ignored":       {env: "", set: true, settings: silent, want: true},
		"settings env beats spawn":     {env: "0", set: true, settings: `{"env": {"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"}}`, want: false},
		"settings env number":          {settings: `{"env": {"CLAUDE_CODE_DISABLE_AUTO_MEMORY": 1}}`, want: false},
		"global env beats spawn":       {env: "1", set: true, global: `{"env": {"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "false"}}`, settings: off, want: true},
		"settings env beats global":    {global: `{"env": {"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"}}`, settings: `{"env": {"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "0"}}`, want: true},
		"other env entries ignored":    {global: `{"env": {"OTHER": "1"}}`, settings: `{"env": {"OTHER": "1"}}`, want: true},
	} {
		if got, err := storeWith(tc.global, tc.settings).AutoMemoryOn(tc.env, tc.set); err != nil || got != tc.want {
			t.Errorf("%s: on = %v, %v; want %v", name, got, err, tc.want)
		}
	}
	if _, err := storeWith("", `{`).AutoMemoryOn("", false); err == nil || !strings.Contains(err.Error(), "settings.json") {
		t.Errorf("a malformed settings file: %v", err)
	}
	if _, err := storeWith(`{`, "").AutoMemoryOn("", false); err == nil || !strings.Contains(err.Error(), ".claude.json") {
		t.Errorf("a malformed ~/.claude.json: %v", err)
	}
}
