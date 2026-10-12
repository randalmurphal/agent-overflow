package threadtools_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/browser"
	"agent-overflow/internal/threadtools"
)

// The vendored pstack skills (assets/skills/pstack) drive Claude and Codex
// threads through the app's tools, so every thread tool and browser tool
// they name has to exist, and no host primitive the port replaced may come
// back in a resync. UPSTREAM.md names the upstream primitives on purpose
// and is the one file not checked.

const skillsTree = "../../assets/skills/pstack"

var (
	threadToolMention  = regexp.MustCompile(`\bthread_[a-z_]+\b`)
	browserToolMention = regexp.MustCompile(`\bbrowser_[a-z_]+\b`)
	markdownLink       = regexp.MustCompile(`\]\(([^)\s]+)\)`)
)

// replacedPrimitives are the upstream host primitives the port rewrote,
// each with what replaced it.
var replacedPrimitives = []struct {
	pattern *regexp.Regexp
	use     string
}{
	{regexp.MustCompile(`\bCursor\b|\.cursor/|\.mdc\b|cursor-team-kit|agent-transcripts`), "the thread tools and the agent's own instructions file"},
	{regexp.MustCompile(`(^|[^A-Za-z0-9_])/loop\b`), "thread_loop"},
	{regexp.MustCompile(`environment: "cloud"|\bcloud agents?\b|subagent_type|run_in_background|\bTask(?: tool| call)\b|` + "`Task`"), "a native subagent or thread_spawn"},
	{regexp.MustCompile(`AskQuestion|SessionStart`), "the provider's own question tool and the agent's instructions file"},
	{regexp.MustCompile(`~/\.(claude|codex|cursor|pi)\b`), "thread_show and thread_search, never a provider home"},
	{regexp.MustCompile(`\bcontrol-(ui|cli)\b`), "ao-browser-tools or the shell"},
	{regexp.MustCompile(`\bgh (pr|api|run|repo|issue)\b|\bglab (mr|api|ci)\b`), "the forge script"},
}

// forgeScript is the one file that runs gh and glab.
const forgeScript = "skills/poteto-mode/scripts/forge"

// keptVerbatim are upstream lines that name a replaced primitive as a
// fact rather than a host step, each by file and a phrase of the line.
var keptVerbatim = map[string]string{
	"README.md": "worked with millions of lines of code at Meta, Netflix, and Cursor",
}

func toolNames(t *testing.T) (tools, params map[string]bool) {
	t.Helper()
	tools, params = map[string]bool{}, map[string]bool{}
	shape := threadtools.Shape{Computers: []threadtools.Computer{{ID: "studio", Name: "Studio"}}}
	for _, schema := range threadtools.New(nil).Tools(shape) {
		tools[schema["name"].(string)] = true
		for name := range schema["inputSchema"].(map[string]any)["properties"].(map[string]any) {
			params[name] = true
		}
	}
	for _, name := range threadtools.ToolNames {
		if !tools[name] {
			t.Fatalf("ToolNames lists %s, which Tools does not return", name)
		}
	}
	for _, name := range browser.ToolNames() {
		tools[name] = true
	}
	return tools, params
}

func skillFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(skillsTree, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(skillsTree, path)
		rel = filepath.ToSlash(rel)
		if rel == "UPSTREAM.md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("read the skills tree: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("the skills tree is empty")
	}
	return files
}

// TestSkillsNameOnlyToolsThatExist fails on a thread_* or browser_* name
// the app does not serve. A thread_* word may also be a parameter of one
// of the thread tools (thread_id), read from the same schemas.
func TestSkillsNameOnlyToolsThatExist(t *testing.T) {
	tools, params := toolNames(t)
	mentioned := map[string]bool{}
	for rel, text := range skillFiles(t) {
		for i, line := range strings.Split(text, "\n") {
			for _, name := range threadToolMention.FindAllString(line, -1) {
				mentioned[name] = true
				if !tools[name] && !params[name] {
					t.Errorf("%s:%d names %s, which is neither a thread tool nor one of their parameters", rel, i+1, name)
				}
			}
			for _, name := range browserToolMention.FindAllString(line, -1) {
				mentioned[name] = true
				if !tools[name] {
					t.Errorf("%s:%d names %s, which is not a browser tool", rel, i+1, name)
				}
			}
		}
	}
	// The port's own mapping: each must be named somewhere.
	for _, name := range []string{"thread_loop", "thread_watch", "thread_spawn", "thread_show", "thread_search", "thread_status", "thread_ask", "browser_screenshot"} {
		if !mentioned[name] {
			t.Errorf("no skill names %s; the port table maps an upstream primitive to it", name)
		}
	}
}

// TestSkillsCarryNoReplacedPrimitive fails when a host primitive the port
// rewrote is back in the tree.
func TestSkillsCarryNoReplacedPrimitive(t *testing.T) {
	files := skillFiles(t)
	paths := slices.Sorted(func(yield func(string) bool) {
		for rel := range files {
			if !yield(rel) {
				return
			}
		}
	})
	for _, rel := range paths {
		for i, line := range strings.Split(files[rel], "\n") {
			for _, primitive := range replacedPrimitives {
				if rel == forgeScript && strings.Contains(primitive.use, "forge script") {
					continue
				}
				if phrase, ok := keptVerbatim[rel]; ok && strings.Contains(line, phrase) {
					continue
				}
				if m := primitive.pattern.FindString(line); m != "" {
					t.Errorf("%s:%d carries %q; the port uses %s", rel, i+1, m, primitive.use)
				}
			}
		}
	}
}

// TestSkillsLinkOnlyFilesThatExist fails on a relative Markdown link to a
// file the tree does not carry, such as a page of the upstream guide the
// port left out.
func TestSkillsLinkOnlyFilesThatExist(t *testing.T) {
	for rel, text := range skillFiles(t) {
		if !strings.HasSuffix(rel, ".md") {
			continue
		}
		for _, m := range markdownLink.FindAllStringSubmatch(text, -1) {
			target, _, _ := strings.Cut(m[1], "#")
			// A template placeholder such as (url) is not a path.
			if !strings.ContainsAny(target, "./") || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.ContainsAny(target, "<>{}") {
				continue
			}
			if _, err := os.Stat(filepath.Join(skillsTree, filepath.Dir(rel), filepath.FromSlash(target))); err != nil {
				t.Errorf("%s links %s, which the tree does not carry", rel, m[1])
			}
		}
	}
}
