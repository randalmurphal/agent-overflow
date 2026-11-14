package claude

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeUserSettings writes content as the user-level settings file of a new
// temp home and returns the home.
func writeUserSettings(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	path := UserSettingsPathForHome(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return home
}

func TestReadCleanupPeriodResolvesTheUserFileOrTheDefault(t *testing.T) {
	unset := CleanupPeriod{Days: DefaultCleanupPeriodDays}
	for _, tc := range []struct {
		name    string
		content string
		want    CleanupPeriod
	}{
		{"key absent", `{"model":"opus"}`, unset},
		{"empty file", "", unset},
		{"whitespace only", " \n", unset},
		{"set", `{"cleanupPeriodDays":365}`, CleanupPeriod{Days: 365, Explicit: true}},
		{"set to the default", `{"cleanupPeriodDays":30}`, CleanupPeriod{Days: 30, Explicit: true}},
		{"minimum", `{"cleanupPeriodDays":1}`, CleanupPeriod{Days: 1, Explicit: true}},
		{"integral float", `{"cleanupPeriodDays":90.0}`, CleanupPeriod{Days: 90, Explicit: true}},
		{"byte order mark", "\xef\xbb\xbf" + `{"cleanupPeriodDays":7}`, CleanupPeriod{Days: 7, Explicit: true}},
		{"beyond int32", `{"cleanupPeriodDays":1e300}`, CleanupPeriod{Days: 1<<31 - 1, Explicit: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadCleanupPeriod(writeUserSettings(t, tc.content), "")
			if err != nil {
				t.Fatalf("ReadCleanupPeriod: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("no settings file", func(t *testing.T) {
		got, err := ReadCleanupPeriod(t.TempDir(), "")
		if err != nil {
			t.Fatalf("ReadCleanupPeriod: %v", err)
		}
		if got != unset {
			t.Fatalf("got %+v, want %+v", got, unset)
		}
	})
}

// A value that cannot be known must be an error, never the default: the
// caller sends no override on an error, and sending one against an unknown
// longer value would delete transcripts the user asked to keep.
func TestReadCleanupPeriodRefusesAnUnknowableValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"malformed json", `{"cleanupPeriodDays":365,`, "parse Claude settings"},
		{"not an object", `[1,2]`, "parse Claude settings"},
		{"json null", `null`, "not a JSON object"},
		{"zero", `{"cleanupPeriodDays":0}`, "whole number of days"},
		{"negative", `{"cleanupPeriodDays":-5}`, "whole number of days"},
		{"fraction", `{"cleanupPeriodDays":30.5}`, "whole number of days"},
		{"string", `{"cleanupPeriodDays":"365"}`, "whole number of days"},
		{"null value", `{"cleanupPeriodDays":null}`, "whole number of days"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadCleanupPeriod(writeUserSettings(t, tc.content), "")
			if err == nil {
				t.Fatalf("got %+v, want an error", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	t.Run("unreadable", func(t *testing.T) {
		home := t.TempDir()
		// A directory where the file belongs: open succeeds, read fails.
		if err := os.MkdirAll(UserSettingsPathForHome(home), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if got, err := ReadCleanupPeriod(home, ""); err == nil {
			t.Fatalf("got %+v, want an error", got)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		home := writeUserSettings(t, `{"cleanupPeriodDays":365,"pad":"`+strings.Repeat("x", maxSettingsFileBytes)+`"}`)
		_, err := ReadCleanupPeriod(home, "")
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("err = %v, want the size refusal", err)
		}
	})
}

// A process started in a workspace reads that workspace's project and local
// settings over the user file, so the period it resolves is the one an
// override is measured against. Measuring against the user file alone would
// send 90 over a project's 365 and shorten it.
func TestReadCleanupPeriodFollowsWorkspaceSettingsPrecedence(t *testing.T) {
	const (
		unset = ""
		user  = "user"
	)
	for _, tc := range []struct {
		name                 string
		user, project, local string // file content, unset writes no file
		want                 CleanupPeriod
	}{
		{"project over user", `{"cleanupPeriodDays":7}`, `{"cleanupPeriodDays":365}`, unset, CleanupPeriod{Days: 365, Explicit: true}},
		{"shorter project over user", `{"cleanupPeriodDays":365}`, `{"cleanupPeriodDays":7}`, unset, CleanupPeriod{Days: 7, Explicit: true}},
		{"local over project", `{"cleanupPeriodDays":7}`, `{"cleanupPeriodDays":365}`, `{"cleanupPeriodDays":60}`, CleanupPeriod{Days: 60, Explicit: true}},
		{"local over user", unset, unset, `{"cleanupPeriodDays":60}`, CleanupPeriod{Days: 60, Explicit: true}},
		{"a file without the key leaves the lower one", `{"cleanupPeriodDays":45}`, `{"model":"opus"}`, `{}`, CleanupPeriod{Days: 45, Explicit: true}},
		{"no file sets it", unset, `{}`, unset, CleanupPeriod{Days: DefaultCleanupPeriodDays}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, workDir := t.TempDir(), t.TempDir()
			for path, content := range map[string]string{
				UserSettingsPathForHome(home):                            tc.user,
				filepath.Join(workDir, ".claude", "settings.json"):       tc.project,
				filepath.Join(workDir, ".claude", "settings.local.json"): tc.local,
			} {
				if content == unset {
					continue
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
			}
			got, err := ReadCleanupPeriod(home, workDir)
			if err != nil {
				t.Fatalf("ReadCleanupPeriod: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("an empty working directory reads the user file alone", func(t *testing.T) {
		got, err := ReadCleanupPeriod(writeUserSettings(t, `{"cleanupPeriodDays":45}`), "")
		if err != nil {
			t.Fatalf("ReadCleanupPeriod: %v", err)
		}
		if want := (CleanupPeriod{Days: 45, Explicit: true}); got != want {
			t.Fatalf("got %+v, want %+v (%s file only)", got, want, user)
		}
	})

	t.Run("an unreadable workspace file makes the period unknown", func(t *testing.T) {
		home, workDir := writeUserSettings(t, `{"cleanupPeriodDays":365}`), t.TempDir()
		project := filepath.Join(workDir, ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(project, []byte(`{"cleanupPeriodDays":`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got, err := ReadCleanupPeriod(home, workDir); err == nil || !strings.Contains(err.Error(), project) {
			t.Fatalf("got %+v, err %v; want an error naming %s", got, err, project)
		}
	})
}

func TestCleanupPeriodOverrideOnlyLengthensTheUsersPeriod(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keepDays int
		user     CleanupPeriod
		want     int
	}{
		{"longer than the default", 90, CleanupPeriod{Days: 30}, 90},
		{"equal to the default", 30, CleanupPeriod{Days: 30}, 0},
		{"shorter than the default", 7, CleanupPeriod{Days: 30}, 0},
		{"longer than the user's", 400, CleanupPeriod{Days: 365, Explicit: true}, 400},
		{"equal to the user's", 365, CleanupPeriod{Days: 365, Explicit: true}, 0},
		{"shorter than the user's", 90, CleanupPeriod{Days: 365, Explicit: true}, 0},
		{"non-positive window", 0, CleanupPeriod{Days: 30}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CleanupPeriodOverride(tc.keepDays, tc.user); got != tc.want {
				t.Fatalf("CleanupPeriodOverride(%d, %+v) = %d, want %d", tc.keepDays, tc.user, got, tc.want)
			}
		})
	}
}

func TestCleanupPeriodArgsRenderOneSettingsPair(t *testing.T) {
	if got, want := CleanupPeriodArgs(90), []string{"--settings", `{"cleanupPeriodDays":90}`}; !slices.Equal(got, want) {
		t.Fatalf("CleanupPeriodArgs(90) = %v, want %v", got, want)
	}
	for _, days := range []int{0, -1} {
		if got := CleanupPeriodArgs(days); got != nil {
			t.Fatalf("CleanupPeriodArgs(%d) = %v, want nil", days, got)
		}
	}
}

// The session block carries the retention beside its other keys, in one
// `--settings` flag.
func TestBuildArgsCarriesCleanupPeriodDaysInTheSettingsBlock(t *testing.T) {
	args := buildArgs(Config{FastMode: true, CleanupPeriodDays: 90}, "", "")
	joined := strings.Join(args, " ")
	if want := `--settings {"fastMode":true,"cleanupPeriodDays":90}`; !strings.Contains(joined, want) {
		t.Fatalf("args missing %q: %v", want, args)
	}
	if got := strings.Count(joined, "--settings"); got != 1 {
		t.Fatalf("expected exactly one --settings flag, got %d in %v", got, args)
	}

	// A value the CLI's schema rejects is never rendered.
	if joined := strings.Join(buildArgs(Config{CleanupPeriodDays: -3}, "", ""), " "); strings.Contains(joined, "--settings") {
		t.Fatalf("a negative period rendered a settings block: %s", joined)
	}
}

func TestProbeArgsCarryCleanupPeriodDays(t *testing.T) {
	args := buildProbeArgs(ProbeConfig{CleanupPeriodDays: 90})
	if !slices.Contains(args, `{"cleanupPeriodDays":90}`) || !slices.Contains(args, "--settings") {
		t.Fatalf("probe args carry no retention: %v", args)
	}
	if args := buildProbeArgs(ProbeConfig{}); slices.Contains(args, "--settings") {
		t.Fatalf("a probe with no retention rendered a settings flag: %v", args)
	}
}
