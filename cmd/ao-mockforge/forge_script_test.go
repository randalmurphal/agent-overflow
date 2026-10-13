package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// The pstack forge script (assets/skills/pstack/.../scripts/forge) run
// against this fake: gh, glab and ssh are links to ao-mockforge, so every
// subcommand on both forges reaches the engine through the CLI path. Run
// with -v for the command log.

var forgeScript, _ = filepath.Abs(filepath.Join("..", "..", "assets", "skills", "pstack", "skills", "poteto-mode", "scripts", "forge"))

// forgeScriptFixture seeds the same pulls on one forge: #1 open with CI
// running and a resolved and an unresolved thread, #2 conflicting, #3
// merged, #4 approved with CI green.
func forgeScriptFixture(forge, project string) forgefake.Fixture {
	approved := forgefake.Review{Author: "erin", State: "APPROVED"}
	threadID := map[string][2]string{
		"github": {"PRRT_open", "PRRT_done"},
		"gitlab": {strings.Repeat("a1", 20), strings.Repeat("b2", 20)},
	}[forge]
	return forgefake.Fixture{Viewer: "ao-viewer", SSHHosts: map[string]string{"work-alias": map[string]string{"github": "github.com", "gitlab": "gitlab.com"}[forge]}, Repos: []forgefake.Repo{{
		Forge: forge, Project: project,
		Pulls: []forgefake.Pull{
			{
				Number: 1, Title: "Add widgets", Body: "Why", Author: "dave", HeadRef: "feature", HeadSHA: strings.Repeat("1", 40),
				Comments: []forgefake.Comment{{Author: "erin", Body: "Looks right overall"}},
				Threads: []forgefake.Thread{
					{ID: threadID[0], Path: "app.go", Line: intPtr(4), Comments: []forgefake.Comment{{Author: "erin", Body: "rename this"}}},
					{ID: threadID[1], Path: "app.go", Line: intPtr(9), Resolved: true, Comments: []forgefake.Comment{{Author: "erin", Body: "done"}, {Author: "dave", Body: "fixed"}}},
				},
				CI: &forgefake.Pipeline{ID: 71, Name: "CI", Jobs: []forgefake.Job{
					{ID: 711, Name: "unit", Status: "success", Log: "unit ok\n"},
					{ID: 712, Name: "e2e", Status: "running", Log: "e2e started\n"},
				}},
			},
			{Number: 2, Title: "Conflicted", Author: "dave", HeadRef: "conflicted", Mergeable: "conflicts"},
			{Number: 3, Title: "Shipped", Author: "erin", HeadRef: "shipped", State: "merged"},
			{
				Number: 4, Title: "Ready", Author: "ao-viewer", HeadRef: "ready", HeadSHA: strings.Repeat("4", 40), Reviews: []forgefake.Review{approved},
				CI: &forgefake.Pipeline{ID: 72, Jobs: []forgefake.Job{{ID: 721, Name: "unit", Status: "success", Log: "green\n"}}},
			},
		},
	}}}
}

type forgeCheckout struct {
	t   *testing.T
	r   *rig
	dir string
	env []string
	// script is the forge script run; forgeScript unless a test swaps
	// in a variant.
	script string
}

func newForgeCheckout(t *testing.T, r *rig, remote, branch string) *forgeCheckout {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"gh", "glab", "ssh"} {
		if err := os.Symlink(fakeBin, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "LC_ALL=C"}
	env = append(env, r.env...)
	f := &forgeCheckout{t: t, r: r, dir: t.TempDir(), env: env, script: forgeScript}
	f.git("init", "-q", "-b", branch)
	f.git("remote", "add", "origin", remote)
	f.git("-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "start")
	return f
}

func (f *forgeCheckout) git(args ...string) {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// run runs the script with stdin and returns its stdout, failing the test
// on a non-zero exit.
func (f *forgeCheckout) run(stdin string, args ...string) string {
	f.t.Helper()
	out, stderr, err := f.try(stdin, args...)
	if err != nil {
		f.t.Fatalf("forge %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return out
}

func (f *forgeCheckout) try(stdin string, args ...string) (string, string, error) {
	f.t.Helper()
	cmd := exec.Command("bash", append([]string{f.script}, args...)...)
	cmd.Dir = f.dir
	cmd.Env = append(f.env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	f.t.Logf("$ forge %s\n%s%s", strings.Join(args, " "), stdout.String(), stderr.String())
	return stdout.String(), stderr.String(), err
}

// json runs the script and decodes its stdout.
func (f *forgeCheckout) json(stdin string, args ...string) any {
	f.t.Helper()
	var v any
	if err := json.Unmarshal([]byte(f.run(stdin, args...)), &v); err != nil {
		f.t.Fatalf("forge %s: stdout is not JSON: %v", strings.Join(args, " "), err)
	}
	return v
}

// pick reads a path of object keys and array indexes out of a decoded
// value.
func pick(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, step := range path {
		switch key := step.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("%v is not an object at %q", v, key)
			}
			v = m[key]
		case int:
			a, ok := v.([]any)
			if !ok || key >= len(a) {
				t.Fatalf("%v has no index %d", v, key)
			}
			v = a[key]
		}
	}
	return v
}

func expect(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", what, got, want)
	}
}

func numbers(t *testing.T, list any) []float64 {
	t.Helper()
	var out []float64
	for _, item := range list.([]any) {
		out = append(out, pick(t, item, "number").(float64))
	}
	return out
}

func TestForgeScript(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("the forge script needs jq on PATH")
	}
	forges := []struct {
		forge, project, host, remote string
		native                       map[string]string
		ref                          gitops.PRReference
	}{
		{"github", "acme/widgets", "github.com", "https://github.com/acme/widgets.git",
			map[string]string{"blocked": "BLOCKED", "clean": "CLEAN", "conflicts": "DIRTY"},
			gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: 1}},
		{"gitlab", "grp/sub/tool", "gitlab.com", "git@gitlab.com:grp/sub/tool.git",
			map[string]string{"blocked": "ci_still_running", "clean": "mergeable", "conflicts": "conflict"},
			gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp/sub", Repo: "tool", Number: 1}},
	}
	for _, fc := range forges {
		t.Run(fc.forge, func(t *testing.T) {
			r := newRig(t, forgeScriptFixture(fc.forge, fc.project))
			f := newForgeCheckout(t, r, fc.remote, "feature")
			threadIDs := map[string][2]string{"github": {"PRRT_open", "PRRT_done"}, "gitlab": {strings.Repeat("a1", 20), strings.Repeat("b2", 20)}}[fc.forge]

			expect(t, "which", f.json("", "which"), map[string]any{"forge": fc.forge, "host": fc.host, "project": fc.project, "cli": map[string]string{"github": "gh", "gitlab": "glab"}[fc.forge]})
			alias := newForgeCheckout(t, r, "git@work-alias:"+fc.project+".git", "main")
			expect(t, "which through an ssh alias", pick(t, alias.json("", "which"), "host"), fc.host)

			view := f.json("", "pr", "view", "1")
			for key, want := range map[string]any{
				"number": 1.0, "title": "Add widgets", "body": "Why", "state": "open", "draft": false, "author": "dave",
				"head": "feature", "head_sha": strings.Repeat("1", 40), "base": "main", "merge": "blocked",
				"merge_native": fc.native["blocked"], "review": "none", "auto_merge": false, "merged_at": nil,
			} {
				expect(t, "pr view 1 ."+key, pick(t, view, key), want)
			}
			expect(t, "pr view on the current branch", pick(t, f.json("", "pr", "view"), "number"), 1.0)
			expect(t, "pr view 4 review", pick(t, f.json("", "pr", "view", "4"), "review"), "approved")
			ready := f.json("", "pr", "view", "4")
			expect(t, "pr view 4 merge", []any{pick(t, ready, "merge"), pick(t, ready, "merge_native")}, []any{"clean", fc.native["clean"]})
			expect(t, "pr view 2 merge", pick(t, f.json("", "pr", "view", "2"), "merge"), "conflicts")
			shipped := f.json("", "pr", "view", "3")
			expect(t, "pr view 3", []any{pick(t, shipped, "state"), pick(t, shipped, "merge"), pick(t, shipped, "merged_at") != nil}, []any{"merged", "none", true})

			expect(t, "pr list", numbers(t, f.json("", "pr", "list")), []float64{4, 2, 1})
			expect(t, "pr list --state all", numbers(t, f.json("", "pr", "list", "--state", "all")), []float64{4, 3, 2, 1})
			expect(t, "pr list --state merged", numbers(t, f.json("", "pr", "list", "--state", "merged")), []float64{3})
			expect(t, "pr list --head feature", numbers(t, f.json("", "pr", "list", "--head", "feature")), []float64{1})
			expect(t, "pr list --author dave", numbers(t, f.json("", "pr", "list", "--state", "all", "--author", "dave")), []float64{2, 1})
			expect(t, "pr list --author @me", numbers(t, f.json("", "pr", "list", "--author", "@me")), []float64{4})
			expect(t, "pr list --limit 2", numbers(t, f.json("", "pr", "list", "--state", "all", "--limit", "2")), []float64{4, 3})
			expect(t, "pr list entry", pick(t, f.json("", "pr", "list", "--head", "feature"), 0, "head"), "feature")

			checks := f.json("", "pr", "checks", "1")
			expect(t, "pr checks state", pick(t, checks, "state"), "pending")
			expect(t, "pr checks sha", pick(t, checks, "sha"), strings.Repeat("1", 40))
			byName := map[string]any{}
			for _, check := range pick(t, checks, "checks").([]any) {
				byName[pick(t, check, "name").(string)] = pick(t, check, "state")
			}
			expect(t, "pr checks", byName, map[string]any{"unit": "pass", "e2e": "pending"})
			expect(t, "pr checks 4 state", pick(t, f.json("", "pr", "checks", "4"), "state"), "pass")
			expect(t, "pr checks 2 (no CI)", f.json("", "pr", "checks", "2"), map[string]any{"number": 2.0, "sha": pick(t, f.json("", "pr", "view", "2"), "head_sha"), "state": "none", "checks": []any{}})
			expect(t, "pr commits 1", f.json("", "pr", "commits", "1"), []any{map[string]any{"sha": strings.Repeat("1", 40), "state": "pending"}})
			expect(t, "pr commits 4", pick(t, f.json("", "pr", "commits", "4"), 0, "state"), "pass")

			threads := f.json("", "pr", "threads", "1").([]any)
			expect(t, "pr threads count", len(threads), 2)
			expect(t, "pr threads[0]", pick(t, threads, 0, "id"), threadIDs[0])
			expect(t, "pr threads[0] path and line", []any{pick(t, threads, 0, "path"), pick(t, threads, 0, "line"), pick(t, threads, 0, "resolved")}, []any{"app.go", 4.0, false})
			expect(t, "pr threads[1] comments", pick(t, threads, 1, "comment_count"), 2.0)
			expect(t, "pr threads[0] comment", pick(t, threads, 0, "comments", 0, "body"), "rename this")
			unresolved := f.json("", "pr", "threads", "1", "--unresolved").([]any)
			expect(t, "pr threads --unresolved", len(unresolved), 1)

			for _, inv := range r.engine.Invocations(0).Invocations {
				if inv.Unhandled {
					t.Errorf("unhandled invocation: %s %q: %s", inv.CLI, inv.Args, inv.Stderr)
				}
			}
		})
	}
}

// TestForgeScriptRefusesAnUnresolvedPull: a read that resolves the
// current branch fails, printing nothing, on a detached HEAD and on a
// branch with no open pull, rather than reading some other pull. It holds
// on a bash without inherit_errexit (bash 3.2, stock macOS), which the
// script stands in for by asking for an option no bash has.
func TestForgeScriptRefusesAnUnresolvedPull(t *testing.T) {
	const shopt = "shopt -s inherit_errexit 2>/dev/null || true\n"
	source, err := os.ReadFile(forgeScript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), shopt) {
		t.Fatalf("the forge script no longer sets inherit_errexit with %q", shopt)
	}
	without := filepath.Join(t.TempDir(), "forge")
	if err := os.WriteFile(without, []byte(strings.Replace(string(source), shopt, "shopt -s no_such_option 2>/dev/null || true\n", 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, fc := range []struct{ name, script, forge, project, remote string }{
		{"github", forgeScript, "github", "acme/widgets", "https://github.com/acme/widgets.git"},
		{"gitlab", forgeScript, "gitlab", "grp/sub/tool", "git@gitlab.com:grp/sub/tool.git"},
		{"github without inherit_errexit", without, "github", "acme/widgets", "https://github.com/acme/widgets.git"},
		{"gitlab without inherit_errexit", without, "gitlab", "grp/sub/tool", "git@gitlab.com:grp/sub/tool.git"},
	} {
		t.Run(fc.name, func(t *testing.T) {
			r := newRig(t, forgeScriptFixture(fc.forge, fc.project))
			detached := newForgeCheckout(t, r, fc.remote, "feature")
			detached.git("switch", "-q", "--detach")
			lonely := newForgeCheckout(t, r, fc.remote, "lonely")
			detached.script, lonely.script = fc.script, fc.script
			reads := [][]string{{"pr", "view"}, {"pr", "checks"}, {"pr", "commits"}, {"pr", "threads"}}
			for _, c := range []struct {
				checkout *forgeCheckout
				reads    [][]string
				stderr   string
			}{
				{detached, slices.Concat(reads, [][]string{{"stack"}}), "HEAD is detached"},
				{lonely, reads, "no open pull request for branch lonely"},
			} {
				for _, args := range c.reads {
					out, stderr, err := c.checkout.try("", args...)
					if err == nil || out != "" || !strings.Contains(stderr, c.stderr) {
						t.Errorf("forge %s = %v, stdout %q, stderr %q; want a failure naming %q", strings.Join(args, " "), err, out, stderr, c.stderr)
					}
				}
			}
			for _, inv := range r.engine.Invocations(0).Invocations {
				if inv.Unhandled {
					t.Errorf("unhandled invocation: %s %q: %s", inv.CLI, inv.Args, inv.Stderr)
				}
			}
		})
	}
}

// TestForgeScriptReadsForgeRemote: the script reads the repository
// FORGE_REMOTE names, not origin's.
func TestForgeScriptReadsForgeRemote(t *testing.T) {
	for _, fc := range []struct{ forge, project, remote string }{
		{"github", "acme/widgets", "https://github.com/acme/widgets.git"},
		{"gitlab", "grp/sub/tool", "git@gitlab.com:grp/sub/tool.git"},
	} {
		t.Run(fc.forge, func(t *testing.T) {
			r := newRig(t, forgeScriptFixture(fc.forge, fc.project))
			f := newForgeCheckout(t, r, otherRemote[fc.forge], "feature")
			f.git("remote", "add", "upstream", fc.remote)
			f.env = append(f.env, "FORGE_REMOTE=upstream")
			expect(t, "which", pick(t, f.json("", "which"), "project"), fc.project)
			expect(t, "pr view on the current branch", pick(t, f.json("", "pr", "view"), "title"), "Add widgets")
			for _, inv := range r.engine.Invocations(0).Invocations {
				if inv.Unhandled {
					t.Errorf("unhandled invocation: %s %q: %s", inv.CLI, inv.Args, inv.Stderr)
				}
			}
		})
	}
}

// otherRemote is an origin naming a repository the fixtures do not seed.
var otherRemote = map[string]string{"github": "https://github.com/other/proj.git", "gitlab": "git@gitlab.com:other/proj.git"}

// TestForgeScriptReadsEveryPage seeds a pull with more jobs and review
// threads than one page (100) holds and reads them through each paginated
// subcommand.
func TestForgeScriptReadsEveryPage(t *testing.T) {
	const count = 101
	for _, fc := range []struct{ forge, project, remote string }{
		{"github", "acme/widgets", "https://github.com/acme/widgets.git"},
		{"gitlab", "grp/sub/tool", "git@gitlab.com:grp/sub/tool.git"},
	} {
		t.Run(fc.forge, func(t *testing.T) {
			pull := forgefake.Pull{Number: 1, Title: "Busy", Author: "dave", HeadRef: "busy", HeadSHA: strings.Repeat("b", 40), CI: &forgefake.Pipeline{ID: 81}}
			for i := range count {
				pull.CI.Jobs = append(pull.CI.Jobs, forgefake.Job{ID: int64(1000 + i), Name: fmt.Sprintf("job-%03d", i), Status: "success"})
				id := fmt.Sprintf("PRRT_%03d", i)
				if fc.forge == "gitlab" {
					id = fmt.Sprintf("%040x", i+1)
				}
				pull.Threads = append(pull.Threads, forgefake.Thread{ID: id, Path: "app.go", Line: intPtr(i + 1), Comments: []forgefake.Comment{{Author: "erin", Body: fmt.Sprintf("thread %d", i)}}})
			}
			r := newRig(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: fc.forge, Project: fc.project, Pulls: []forgefake.Pull{pull}}}})
			f := newForgeCheckout(t, r, fc.remote, "busy")

			for _, read := range []struct {
				args  []string
				check func(t *testing.T, out any)
			}{
				{[]string{"pr", "checks"}, func(t *testing.T, out any) {
					expect(t, "pr checks", []any{len(pick(t, out, "checks").([]any)), pick(t, out, "state")}, []any{count, "pass"})
				}},
				{[]string{"pr", "threads"}, func(t *testing.T, out any) {
					expect(t, "pr threads", []any{len(out.([]any)), pick(t, out, count-1, "comments", 0, "body")}, []any{count, fmt.Sprintf("thread %d", count-1)})
				}},
				{[]string{"pr", "commits"}, func(t *testing.T, out any) {
					expect(t, "pr commits", out, []any{map[string]any{"sha": strings.Repeat("b", 40), "state": "pass"}})
				}},
			} {
				t.Run(strings.Join(read.args, " "), func(t *testing.T) {
					f.t = t
					read.check(t, f.json("", read.args...))
				})
			}
			f.t = t

			for _, inv := range r.engine.Invocations(0).Invocations {
				if inv.Unhandled {
					t.Errorf("unhandled invocation: %s %q: %s", inv.CLI, inv.Args, inv.Stderr)
				}
			}
		})
	}
}

// stackFixture seeds stacks on one forge, each named by its top branch:
// stack/three on stack/two on stack/one on main, where stack/two also
// has an older closed pull; old/top on a merged old/base; abandoned,
// closed with nothing open; dangling, whose base gone has no pull; and
// loop/a and loop/b, based on each other.
func stackFixture(forge, project string) forgefake.Fixture {
	pull := func(n int, head, base, state string) forgefake.Pull {
		return forgefake.Pull{Number: n, Title: head, Author: "dave", HeadRef: head, BaseRef: base, State: state, HeadSHA: strings.Repeat(string(rune('a'+n%6)), 40)}
	}
	return forgefake.Fixture{Repos: []forgefake.Repo{{Forge: forge, Project: project, Pulls: []forgefake.Pull{
		pull(9, "stack/two", "main", "closed"),
		pull(11, "stack/one", "main", ""),
		pull(12, "stack/two", "stack/one", ""),
		pull(13, "stack/three", "stack/two", ""),
		pull(20, "old/base", "main", "merged"),
		pull(21, "old/top", "old/base", ""),
		pull(22, "abandoned", "main", "closed"),
		pull(23, "dangling", "gone", ""),
		pull(24, "loop/a", "loop/b", ""),
		pull(25, "loop/b", "loop/a", ""),
	}}}}
}

func TestForgeScriptStack(t *testing.T) {
	for _, fc := range []struct{ forge, project, remote string }{
		{"github", "acme/widgets", "https://github.com/acme/widgets.git"},
		{"gitlab", "grp/sub/tool", "git@gitlab.com:grp/sub/tool.git"},
	} {
		t.Run(fc.forge, func(t *testing.T) {
			r := newRig(t, stackFixture(fc.forge, fc.project))
			f := newForgeCheckout(t, r, fc.remote, "stack/three")

			stack := f.json("", "stack")
			expect(t, "stack of three", numbers(t, stack), []float64{11, 12, 13})
			for i, want := range [][3]any{{"stack/one", "main", "open"}, {"stack/two", "stack/one", "open"}, {"stack/three", "stack/two", "open"}} {
				expect(t, "stack entry", []any{pick(t, stack, i, "head"), pick(t, stack, i, "base"), pick(t, stack, i, "state")}, []any{want[0], want[1], want[2]})
			}
			expect(t, "stack entry sha", pick(t, stack, 0, "head_sha"), strings.Repeat("f", 40))
			expect(t, "stack of a named branch", numbers(t, f.json("", "stack", "stack/two")), []float64{11, 12})
			expect(t, "stack over a merged base", f.json("", "stack", "old/top").([]any)[0].(map[string]any)["state"], "merged")
			expect(t, "stack over a merged base", numbers(t, f.json("", "stack", "old/top")), []float64{20, 21})
			expect(t, "stack of a closed pull", numbers(t, f.json("", "stack", "abandoned")), []float64{22})
			expect(t, "stack of the default branch", f.json("", "stack", "main"), []any{})

			for _, missing := range []struct{ branch, named string }{{"orphan", "orphan"}, {"dangling", "gone"}} {
				out, stderr, err := f.try("", "stack", missing.branch)
				exit, _ := err.(*exec.ExitError)
				if exit == nil || exit.ExitCode() != 3 || out != "" || !strings.Contains(stderr, "branch "+missing.named+" has no pull request") {
					t.Errorf("stack %s = exit %v, stdout %q, stderr %q; want exit 3 naming %s", missing.branch, err, out, stderr, missing.named)
				}
			}
			if out, stderr, err := f.try("", "stack", "loop/a"); err == nil || out != "" || !strings.Contains(stderr, "cycle") {
				t.Errorf("stack loop/a = %v, stdout %q, stderr %q; want a cycle error", err, out, stderr)
			}

			for _, inv := range r.engine.Invocations(0).Invocations {
				if inv.Unhandled {
					t.Errorf("unhandled invocation: %s %q: %s", inv.CLI, inv.Args, inv.Stderr)
				}
			}
		})
	}
}

// TestForgeScriptHelp: help asked for prints the usage on stdout and
// exits 0; a call the script cannot run prints it on stderr and exits 2.
// Help and a missing command need no checkout, so they also run in a
// directory that is not one.
func TestForgeScriptHelp(t *testing.T) {
	r := newRig(t, forgeScriptFixture("github", "acme/widgets"))
	f := newForgeCheckout(t, r, "https://github.com/acme/widgets.git", "main")
	outside := *f
	outside.dir = t.TempDir()
	outside.env = append(append([]string(nil), f.env...), "GIT_CEILING_DIRECTORIES="+filepath.Dir(outside.dir))
	// Help for every command, and calls that name no command the script
	// has, including the subcommands it no longer carries.
	help := [][]string{
		{"--help"}, {"-h"}, {"help"}, {"which", "--help"}, {"which", "-h"}, {"stack", "--help"}, {"stack", "main", "-h"},
		{"pr", "--help"}, {"pr", "help"}, {"pr", "view", "--help"}, {"pr", "view", "1", "-h"}, {"pr", "list", "--help"},
		{"pr", "list", "--state", "all", "-h"}, {"pr", "checks", "-h"}, {"pr", "commits", "--help"}, {"pr", "threads", "1", "--help"},
	}
	misuse := [][]string{
		{}, {"bogus"}, {"which", "extra"}, {"stack", "a", "b"}, {"pr"}, {"pr", "bogus"},
		{"pr", "merge", "1"}, {"pr", "create"}, {"pr", "comment", "1"}, {"ci"}, {"ci", "status"},
	}
	for _, c := range []struct {
		name string
		f    *forgeCheckout
	}{
		{"in a checkout", f},
		{"outside a checkout", &outside},
	} {
		for _, args := range help {
			out, stderr, err := c.f.try("", args...)
			if err != nil || !strings.Contains(out, "forge pr view [<n>]") || stderr != "" {
				t.Errorf("%s: forge %s = %v, stdout %q, stderr %q; want the usage on stdout and exit 0", c.name, strings.Join(args, " "), err, out, stderr)
			}
		}
		for _, args := range misuse {
			out, stderr, err := c.f.try("", args...)
			exit, _ := err.(*exec.ExitError)
			if exit == nil || exit.ExitCode() != 2 || out != "" || !strings.Contains(stderr, "forge pr view [<n>]") {
				t.Errorf("%s: forge %s = %v, stdout %q, stderr %q; want the usage on stderr and exit 2", c.name, strings.Join(args, " "), err, out, stderr)
			}
		}
	}
	// The directory really is outside a checkout: a call that needs the
	// remote fails there.
	if _, _, err := outside.try("", "which"); err == nil {
		t.Error("forge which outside a checkout succeeded")
	}
}

func TestForgeScriptRefusesAnUnknownHost(t *testing.T) {
	r := newRig(t, forgeScriptFixture("github", "acme/widgets"))
	f := newForgeCheckout(t, r, "https://git.example.invalid/acme/widgets.git", "main")
	if _, stderr, err := f.try("", "which"); err == nil || !strings.Contains(stderr, "set FORGE=github or FORGE=gitlab") {
		t.Fatalf("unknown host = %v %s", err, stderr)
	}
	f.env = append(f.env, "FORGE=gitlab")
	expect(t, "FORGE=gitlab", pick(t, f.json("", "which"), "forge"), "gitlab")
}
