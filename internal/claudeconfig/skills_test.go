package claudeconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSkill creates <root>/skills/<dir>/SKILL.md with the given body.
func writeSkill(t *testing.T, root, dir, body string) {
	t.Helper()
	skillDir := filepath.Join(root, "skills", dir)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", skillDir, err)
	}
	writeFile(t, filepath.Join(skillDir, "SKILL.md"), body)
}

func skillNames(skills []Skill) []string {
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	return names
}

func TestListSkills_UserAndProjectTiers(t *testing.T) {
	f := newPluginFixture(t, "", "")
	workspace := t.TempDir()
	writeSkill(t, f.home, "review-code", "---\nname: review-code\ndescription: Reviews code carefully.\n---\n\nBody.\n")
	writeSkill(t, filepath.Join(workspace, ".claude"), "deploy", "---\nname: deploy\ndescription: Deploys this project.\n---\n")

	skills, err := f.store.ListSkills(workspace)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if got := skillNames(skills); len(got) != 2 || got[0] != "deploy" || got[1] != "review-code" {
		t.Fatalf("names = %v, want [deploy review-code]", got)
	}
	if skills[0].Scope != SkillScopeProject || skills[1].Scope != SkillScopeUser {
		t.Fatalf("scopes = %q/%q, want project/user", skills[0].Scope, skills[1].Scope)
	}
	if skills[0].Description != "Deploys this project." {
		t.Fatalf("description = %q", skills[0].Description)
	}
}

func TestListSkills_ProjectWinsNameCollision(t *testing.T) {
	f := newPluginFixture(t, "", "")
	workspace := t.TempDir()
	writeSkill(t, f.home, "review", "---\nname: review\ndescription: User tier.\n---\n")
	writeSkill(t, filepath.Join(workspace, ".claude"), "review", "---\nname: review\ndescription: Project tier.\n---\n")

	skills, err := f.store.ListSkills(workspace)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(skills) != 1 || skills[0].Scope != SkillScopeProject || skills[0].Description != "Project tier." {
		t.Fatalf("skills = %+v, want the single project-tier entry", skills)
	}
}

func TestListSkills_FrontmatterFallbacksAndMalformed(t *testing.T) {
	f := newPluginFixture(t, "", "")
	// No frontmatter at all: directory name is the skill name.
	writeSkill(t, f.home, "fenceless", "Just prose, no fence.\n")
	// A horizontal rule is not a fence.
	writeSkill(t, f.home, "hr-start", "----\nStill prose.\n")
	// Frontmatter without a name: directory-name fallback.
	writeSkill(t, f.home, "unnamed", "---\ndescription: Has only a description.\n---\n")
	// Unclosed fence: the CLI would not load it; neither do we.
	writeSkill(t, f.home, "unclosed", "---\nname: broken\n")
	// Invalid YAML inside the fence: skipped.
	writeSkill(t, f.home, "badyaml", "---\nname: [unclosed\n---\n")
	// A directory without SKILL.md contributes nothing.
	if err := os.MkdirAll(filepath.Join(f.home, "skills", "empty-dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	skills, err := f.store.ListSkills("")
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	got := skillNames(skills)
	want := []string{"fenceless", "hr-start", "unnamed"}
	if len(got) != len(want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %v, want %v", got, want)
		}
	}
	for _, s := range skills {
		if s.Name == "unnamed" && s.Description != "Has only a description." {
			t.Fatalf("unnamed description = %q", s.Description)
		}
	}
}

func TestListSkills_PluginSkillsAreNamespacedAndGated(t *testing.T) {
	f := newPluginFixture(t, `{
  "enabledPlugins": {
    "toolkit@market": true,
    "muted@market": false
  }
}`, "")
	toolkitDir := f.installPlugin(t, "toolkit", "")
	mutedDir := f.installPlugin(t, "muted", "")
	writeSkill(t, toolkitDir, "lint-fast", "---\nname: lint-fast\ndescription: Lints fast.\n---\n")
	writeSkill(t, mutedDir, "hidden", "---\nname: hidden\n---\n")
	writeFile(t, filepath.Join(f.home, "plugins", "installed_plugins.json"), `{
  "version": 2,
  "plugins": {
    "toolkit@market": [{"scope": "user", "installPath": `+quoteJSON(toolkitDir)+`}],
    "muted@market": [{"scope": "user", "installPath": `+quoteJSON(mutedDir)+`}]
  }
}`)

	skills, err := f.store.ListSkills("/anywhere")
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if got := skillNames(skills); len(got) != 1 || got[0] != "toolkit:lint-fast" {
		t.Fatalf("names = %v, want [toolkit:lint-fast]", got)
	}
	if skills[0].Scope != SkillScopePlugin {
		t.Fatalf("scope = %q, want plugin", skills[0].Scope)
	}
}

func TestListSkills_MissingDirectoriesAreEmptyAnswers(t *testing.T) {
	f := newPluginFixture(t, "", "")
	skills, err := f.store.ListSkills(t.TempDir())
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(skills) != 0 {
		t.Fatalf("skills = %+v, want none", skills)
	}
}
