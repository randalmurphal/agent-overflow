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
// subcommand on both forges reaches the engine through the CLI path, and a
// write is checked by reading it back, through the script or through the
// app's own forge reads. Run with -v for the command log.

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
			expect(t, "pr merge-state 4", f.json("", "pr", "merge-state", "4"), map[string]any{"number": 4.0, "state": "open", "merge": "clean", "merge_native": fc.native["clean"], "ready": true})
			expect(t, "pr merge-state 2", pick(t, f.json("", "pr", "merge-state", "2"), "merge"), "conflicts")
			expect(t, "pr merge-state 3", pick(t, f.json("", "pr", "merge-state", "3"), "ready"), false)

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

			const reply = "Renamed in the next push; `$(touch pwned)` stays text"
			expect(t, "pr reply id", pick(t, f.json(reply, "pr", "reply", "1", threadIDs[0], "--body-file", "-"), "id") != nil, true)
			after := f.json("", "pr", "threads", "1").([]any)
			expect(t, "thread after reply", []any{pick(t, after, 0, "comment_count"), pick(t, after, 0, "comments", 1, "body"), pick(t, after, 0, "comments", 1, "author")}, []any{2.0, reply, "ao-viewer"})

			conversation := f.json("", "pr", "comments", "1").([]any)
			expect(t, "pr comments", []any{len(conversation), pick(t, conversation, 0, "author"), pick(t, conversation, 0, "body")}, []any{1, "erin", "Looks right overall"})

			const note = "Status: CI is running.\nNext: merge when green."
			body := filepath.Join(t.TempDir(), "note.md")
			if err := os.WriteFile(body, []byte(note), 0o600); err != nil {
				t.Fatal(err)
			}
			comment := f.json("", "pr", "comment", "1", "--body-file", body)
			if id, _ := pick(t, comment, "id").(float64); id == 0 || !strings.Contains(pick(t, comment, "url").(string), "/1") {
				t.Errorf("pr comment = %v", comment)
			}
			read, err := r.core.ReadPR(t.Context(), fc.ref, gitops.PRReadParts{Threads: true}, nil, nil)
			if err != nil {
				t.Fatalf("ReadPR after comment: %v", err)
			}
			found := false
			for _, thread := range read.Threads {
				for _, c := range thread.Comments {
					found = found || (thread.Path == "" && c.Body == note)
				}
			}
			expect(t, "the comment in the app's read", found, true)
			conversation = f.json("", "pr", "comments", "1").([]any)
			expect(t, "pr comments after a comment", []any{len(conversation), pick(t, conversation, 1, "body"), pick(t, conversation, 1, "url")}, []any{2, note, pick(t, comment, "url")})
			if _, stderr, err := f.try("", "pr", "comment", "1", "--body-file", "-"); err == nil || !strings.Contains(stderr, "body is empty") {
				t.Errorf("an empty comment body was sent: %v %s", err, stderr)
			}
			for _, inv := range r.engine.Invocations(0).Invocations {
				if strings.Contains(strings.Join(inv.Args, " "), "Renamed in the next push") || strings.Contains(strings.Join(inv.Args, " "), "CI is running") {
					t.Errorf("a body reached the command line: %q", inv.Args)
				}
			}

			expect(t, "pr edit --base", pick(t, f.json("", "pr", "edit", "1", "--base", "develop"), "base"), "develop")
			armed := f.json("", "pr", "merge", "1", "--auto")
			expect(t, "pr merge --auto on pending CI", []any{pick(t, armed, "state"), pick(t, armed, "auto_merge")}, []any{"open", true})
			if _, _, err := f.try("", "pr", "merge", "2"); err == nil {
				t.Error("merging a conflicting pull succeeded")
			}
			merged := f.json("", "pr", "merge", "4")
			expect(t, "pr merge 4 state", pick(t, merged, "state"), "merged")
			if pick(t, merged, "merged_at") == nil {
				t.Error("a merged pull has no merged_at")
			}

			if fc.forge == "gitlab" {
				// GitLab before 17.11 reads only merge_when_pipeline_succeeds.
				armedBodies := 0
				for _, inv := range r.engine.Invocations(0).Invocations {
					if slices.ContainsFunc(inv.Args, func(arg string) bool { return strings.HasSuffix(arg, "/merge_requests/1/merge") }) {
						var body map[string]any
						if err := json.Unmarshal([]byte(inv.Stdin), &body); err != nil {
							t.Fatalf("merge body %q: %v", inv.Stdin, err)
						}
						expect(t, "merge --auto body", []any{body["auto_merge"], body["merge_when_pipeline_succeeds"]}, []any{true, true})
						armedBodies++
					}
				}
				expect(t, "merge --auto requests", armedBodies, 1)
			}

			status := f.json("", "ci", "status")
			expect(t, "ci status", []any{pick(t, status, "ref"), pick(t, status, "sha"), pick(t, status, "state")}, []any{"feature", strings.Repeat("1", 40), "pending"})
			jobs := map[string]any{}
			for _, job := range pick(t, status, "runs", 0, "jobs").([]any) {
				jobs[pick(t, job, "name").(string)] = pick(t, job, "id")
			}
			expect(t, "ci status jobs", jobs, map[string]any{"unit": 711.0, "e2e": 712.0})
			expect(t, "ci status ready", pick(t, f.json("", "ci", "status", "ready"), "state"), "pass")
			expect(t, "ci status of a branch without CI", f.json("", "ci", "status", "nowhere"), map[string]any{"ref": "nowhere", "sha": nil, "state": "none", "runs": []any{}})
			expect(t, "ci log", strings.TrimPrefix(f.run("", "ci", "log", "711"), "\ufeff"), "unit ok\n")
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
			reads := [][]string{{"pr", "view"}, {"pr", "merge-state"}, {"pr", "checks"}, {"pr", "commits"}, {"pr", "comments"}, {"pr", "threads"}}
			for _, c := range []struct {
				checkout *forgeCheckout
				reads    [][]string
				stderr   string
			}{
				{detached, slices.Concat(reads, [][]string{{"ci", "status"}, {"stack"}}), "HEAD is detached"},
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

// TestForgeScriptCreatesOnForgeRemote: create opens the pull in the
// repository FORGE_REMOTE names, not in origin's.
func TestForgeScriptCreatesOnForgeRemote(t *testing.T) {
	for _, fc := range []struct{ forge, project, remote string }{
		{"github", "acme/widgets", "https://github.com/acme/widgets.git"},
		{"gitlab", "grp/sub/tool", "git@gitlab.com:grp/sub/tool.git"},
	} {
		t.Run(fc.forge, func(t *testing.T) {
			r := newRig(t, forgeScriptFixture(fc.forge, fc.project))
			topic := newForgeCheckout(t, r, otherRemote[fc.forge], "topic")
			topic.git("remote", "add", "upstream", fc.remote)
			topic.env = append(topic.env, "FORGE_REMOTE=upstream")
			created := topic.json("Adds a topic.", "pr", "create", "--title", "Topic", "--body-file", "-", "--draft")
			expect(t, "pr create number", pick(t, created, "number"), 5.0)
			createdView := topic.json("", "pr", "view")
			expect(t, "created pull", []any{pick(t, createdView, "number"), pick(t, createdView, "head"), pick(t, createdView, "draft"), pick(t, createdView, "body")}, []any{5.0, "topic", true, "Adds a topic."})
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

// TestForgeScriptReadsEveryPage seeds a pull with more jobs, review
// threads and conversation comments than one page (100) holds and reads
// them through each paginated subcommand.
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
				pull.Comments = append(pull.Comments, forgefake.Comment{Author: "erin", Body: fmt.Sprintf("comment %d", i)})
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
				{[]string{"ci", "status"}, func(t *testing.T, out any) {
					expect(t, "ci status jobs", []any{len(pick(t, out, "runs", 0, "jobs").([]any)), pick(t, out, "state")}, []any{count, "pass"})
				}},
				{[]string{"pr", "threads"}, func(t *testing.T, out any) {
					expect(t, "pr threads", []any{len(out.([]any)), pick(t, out, count-1, "comments", 0, "body")}, []any{count, fmt.Sprintf("thread %d", count-1)})
				}},
				{[]string{"pr", "comments"}, func(t *testing.T, out any) {
					expect(t, "pr comments", []any{len(out.([]any)), pick(t, out, count-1, "body")}, []any{count, fmt.Sprintf("comment %d", count-1)})
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

func TestForgeScriptRefusesAnUnknownHost(t *testing.T) {
	r := newRig(t, forgeScriptFixture("github", "acme/widgets"))
	f := newForgeCheckout(t, r, "https://git.example.invalid/acme/widgets.git", "main")
	if _, stderr, err := f.try("", "which"); err == nil || !strings.Contains(stderr, "set FORGE=github or FORGE=gitlab") {
		t.Fatalf("unknown host = %v %s", err, stderr)
	}
	f.env = append(f.env, "FORGE=gitlab")
	expect(t, "FORGE=gitlab", pick(t, f.json("", "which"), "forge"), "gitlab")
}
