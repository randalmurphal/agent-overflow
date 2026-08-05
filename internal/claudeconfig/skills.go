package claudeconfig

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill is one Claude Code skill discovered on disk. Name is the
// invocation word (plugin skills carry the CLI's `<plugin>:<skill>`
// qualified form); Scope records which tier contributed it.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Scope       string `json:"scope"` // "user" | "project" | "plugin"
}

// Skill scopes, ascending precedence for name collisions.
const (
	SkillScopeUser    = "user"
	SkillScopeProject = "project"
	SkillScopePlugin  = "plugin"
)

// skillFrontmatterLimit bounds how much of a SKILL.md is read looking
// for the closing frontmatter fence. Skill bodies can be arbitrarily
// large; frontmatter that hasn't closed within this window is treated
// as malformed.
const skillFrontmatterLimit = 64 * 1024

// ListSkills enumerates the skills a Claude session started in
// workspacePath would load, without spawning anything: the user tier
// (<claude home>/skills), the project tier (<workspace>/.claude/skills),
// and each enabled plugin's skills directory. The zero-token account
// probe runs --safe-mode and reports no skills, so this filesystem read
// is what lets a composer menu list them before any session exists; a
// live session's `system/init` command list remains authoritative once
// it arrives.
//
// A project skill wins a name collision with a user skill — the same
// precedence the CLI applies. Plugin skills are namespaced
// `<plugin>:<skill>`, so they cannot collide with either.
//
// Per-skill problems (malformed frontmatter) skip that skill, mirroring
// the CLI not loading it. Missing directories are normal empty answers;
// any other filesystem error is surfaced.
func (s *Store) ListSkills(workspacePath string) ([]Skill, error) {
	byName := make(map[string]Skill)
	if err := collectSkillDir(filepath.Join(s.home, "skills"), SkillScopeUser, "", byName); err != nil {
		return nil, err
	}
	if workspacePath != "" {
		// Project skills load into the same namespace and override user
		// skills of the same name, so they land in the same map second.
		if err := collectSkillDir(filepath.Join(workspacePath, ".claude", "skills"), SkillScopeProject, "", byName); err != nil {
			return nil, err
		}
	}

	pluginSkills, err := s.pluginSkills(workspacePath)
	if err != nil {
		return nil, err
	}

	out := make([]Skill, 0, len(byName)+len(pluginSkills))
	for _, skill := range byName {
		out = append(out, skill)
	}
	out = append(out, pluginSkills...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// pluginSkills enumerates the skills each enabled plugin ships under
// its installation's skills/ directory, using the same enablement and
// relevance rules as pluginServers.
func (s *Store) pluginSkills(workspacePath string) ([]Skill, error) {
	enabled, err := s.enabledPlugins(workspacePath)
	if err != nil {
		return nil, err
	}
	if len(enabled) == 0 {
		return nil, nil
	}
	installs, err := s.installedPlugins()
	if err != nil {
		return nil, err
	}
	var out []Skill
	for pluginID, entries := range installs {
		if !enabled[pluginID] {
			continue
		}
		pluginName, _, ok := strings.Cut(pluginID, "@")
		if !ok || pluginName == "" {
			continue
		}
		installPath := ""
		for _, entry := range entries {
			if entry.relevantTo(workspacePath) {
				installPath = entry.InstallPath
				break
			}
		}
		if installPath == "" {
			continue
		}
		byName := make(map[string]Skill)
		if err := collectSkillDir(filepath.Join(installPath, "skills"), SkillScopePlugin, pluginName, byName); err != nil {
			return nil, err
		}
		for _, skill := range byName {
			out = append(out, skill)
		}
	}
	return out, nil
}

// collectSkillDir scans one skills directory: each subdirectory holding
// a SKILL.md contributes one skill, overwriting a same-named entry from
// an earlier (lower-precedence) call. namePrefix qualifies plugin
// skills as `<plugin>:<skill>`.
func collectSkillDir(dir, scope, pluginName string, byName map[string]Skill) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skill, ok, err := readSkillFile(filepath.Join(dir, entry.Name(), "SKILL.md"), entry.Name())
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		skill.Scope = scope
		if pluginName != "" {
			skill.Name = pluginName + ":" + skill.Name
		}
		byName[skill.Name] = skill
	}
	return nil
}

// readSkillFile parses one SKILL.md's YAML frontmatter. ok=false means
// the directory holds no skill (no SKILL.md) or one the CLI would not
// load (malformed frontmatter). A file WITHOUT a frontmatter fence is
// still a skill — the directory name is its name, description empty.
func readSkillFile(path, dirName string) (Skill, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Skill{}, false, nil
		}
		return Skill{}, false, err
	}
	defer f.Close()
	head, err := io.ReadAll(io.LimitReader(f, skillFrontmatterLimit))
	if err != nil {
		return Skill{}, false, err
	}

	block, hasFrontmatter, malformed := frontmatterBlock(head)
	if malformed {
		// A fence that never closes (within the read window) is
		// malformed, not fenceless — the CLI would not load it.
		return Skill{}, false, nil
	}
	if !hasFrontmatter {
		return Skill{Name: dirName}, true, nil
	}

	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(block, &meta); err != nil {
		return Skill{}, false, nil
	}
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		name = dirName
	}
	return Skill{Name: name, Description: strings.TrimSpace(meta.Description)}, true, nil
}

// frontmatterBlock extracts the YAML between a leading `---` fence line
// and its closing `---` (or `...`) line. found=false with
// malformed=false means the file simply has no frontmatter (including a
// `----` horizontal rule, which is not a fence); malformed=true means a
// fence opened but never closed within the read window.
func frontmatterBlock(data []byte) (block []byte, found, malformed bool) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	rest, cut := bytes.CutPrefix(data, []byte("---"))
	if !cut || !isFenceLineEnd(rest) {
		return nil, false, false
	}
	idx := bytes.IndexByte(rest, '\n')
	if idx < 0 {
		return nil, false, true
	}
	rest = rest[idx+1:]
	body := rest
	for offset := 0; ; {
		lineEnd := bytes.IndexByte(rest, '\n')
		line := rest
		if lineEnd >= 0 {
			line = rest[:lineEnd]
		}
		if trimmed := bytes.TrimRight(line, " \t\r"); bytes.Equal(trimmed, []byte("---")) || bytes.Equal(trimmed, []byte("...")) {
			return body[:offset], true, false
		}
		if lineEnd < 0 {
			return nil, false, true
		}
		rest = rest[lineEnd+1:]
		offset += lineEnd + 1
	}
}

// isFenceLineEnd reports whether the bytes after a leading `---` finish
// that fence line: nothing but spaces/tabs/CR before the newline (or
// end of input).
func isFenceLineEnd(rest []byte) bool {
	idx := bytes.IndexByte(rest, '\n')
	line := rest
	if idx >= 0 {
		line = rest[:idx]
	}
	return len(bytes.TrimRight(line, " \t\r")) == 0
}
