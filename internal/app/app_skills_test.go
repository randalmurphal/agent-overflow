package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/codexapp"
	"agent-overflow/internal/codexskills"
	"agent-overflow/internal/eventchan"
)

// The Skills bindings resolve each provider's home through the provider-home
// seam, install into `<home>/skills`, and say so to every client.
func TestSkillsBindingsInstallIntoTheProviderHomes(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	home := testProviderHome(t, f.app)

	library, err := f.app.GetSkillsLibrary()
	if err != nil {
		t.Fatalf("GetSkillsLibrary: %v", err)
	}
	if library.Version == "" || len(library.Skills) == 0 || len(library.Homes) != 2 {
		t.Fatalf("library = %+v", library)
	}
	for _, row := range library.Homes {
		if row.Installed || row.Error != "" {
			t.Fatalf("fresh home %s = %+v", row.Provider, row)
		}
	}

	if err := f.app.InstallSkills("codex"); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "skills", library.Skills[0], "SKILL.md")); err != nil {
		t.Fatalf("installed skill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills")); !os.IsNotExist(err) {
		t.Fatalf("an install for Codex touched Claude's home: %v", err)
	}
	library, _ = f.app.GetSkillsLibrary()
	if got := library.Homes[1]; got.Provider != "codex" || !got.Installed || !got.Current {
		t.Fatalf("codex after install = %+v", got)
	}
	emitted := 0
	for _, call := range f.events.snapshot() {
		if call.Channel == eventchan.SkillsChanged.String() {
			emitted++
		}
	}
	if emitted != 1 {
		t.Fatalf("skills:changed frames = %d, want 1", emitted)
	}

	if err := f.app.RemoveSkills("codex"); err != nil {
		t.Fatalf("RemoveSkills: %v", err)
	}
	// Only the lock file stays: removing a lock file races its users.
	if entries, err := os.ReadDir(filepath.Join(home, ".codex", "skills")); err != nil || len(entries) != 1 || entries[0].Name() != ".agent-overflow-pstack.lock" {
		t.Fatalf("after remove: %v %v", entries, err)
	}
	if err := f.app.UpdateSkills("claude"); err == nil {
		t.Fatal("Update of a home with no install was not refused")
	}
	if err := f.app.InstallSkills("cursor"); err == nil {
		t.Fatal("an unknown provider was not refused")
	}
}

// The composer's Codex skill list is cached per workspace; a change to the
// Codex home drops it so the next read sees the installed skills.
func TestInstallSkillsDropsTheCachedCodexSkillList(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	cache := codexskills.New()
	f.app.codexAppOnce.Do(func() {
		f.app.codexApp = codexapp.New(codexapp.Deps{Binary: func() string { return "codex-test" }, SkillsCache: cache})
	})
	key := codexskills.Key("codex-test", t.TempDir())
	reads := 0
	fetch := func(context.Context) (codexskills.CwdSkills, error) {
		reads++
		return codexskills.CwdSkills{}, nil
	}
	if _, err := cache.Get(context.Background(), key, fetch); err != nil {
		t.Fatal(err)
	}
	if err := f.app.InstallSkills("codex"); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	if _, err := cache.Get(context.Background(), key, fetch); err != nil {
		t.Fatal(err)
	}
	if reads != 2 {
		t.Fatalf("skill list reads = %d, want 2: the install left the cached list in place", reads)
	}
}
