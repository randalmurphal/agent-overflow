package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// authoredRepo is a repository whose user.email is me@example.com, with
// commit making a commit by author at authored, committed at committed,
// and run running git in it.
func authoredRepo(t *testing.T) (repo string, commit func(author string, authored, committed time.Time, subject string), run func(args ...string)) {
	t.Helper()
	repo = t.TempDir()
	git := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(nil, "init", "-q", "-b", "main")
	git(nil, "config", "user.email", "me@example.com")
	git(nil, "config", "user.name", "Me")
	n := 0
	commit = func(author string, authored, committed time.Time, subject string) {
		t.Helper()
		n++
		if err := os.WriteFile(filepath.Join(repo, "f"+strconv.Itoa(n)), []byte(subject), 0o644); err != nil {
			t.Fatal(err)
		}
		git(nil, "add", "-A")
		git([]string{
			"GIT_AUTHOR_NAME=" + author, "GIT_AUTHOR_EMAIL=" + author,
			"GIT_AUTHOR_DATE=" + strconv.FormatInt(authored.Unix(), 10) + " +0000",
			"GIT_COMMITTER_DATE=" + strconv.FormatInt(committed.Unix(), 10) + " +0000",
		}, "commit", "-q", "-m", subject)
	}
	return repo, commit, func(args ...string) { git(nil, args...) }
}

// The commits are those the repository's user.email authored in the
// range on any local branch, by author time: not another author's, not
// one whose email only contains the person's, not a merge, and not one
// authored before the range and rebased into it.
func TestAuthoredCommitsAreThePersonsInTheRange(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 7)
	at := func(h int) time.Time { return from.Add(time.Duration(h) * time.Hour) }
	repo, commit, run := authoredRepo(t)
	me, other, longer := "me@example.com", "other@example.com", "xme@example.com"
	commit(me, at(-30), at(-30), "before the range")
	commit(me, at(10), at(10), "a\ttab and ü")
	commit(other, at(11), at(11), "another's")
	commit(longer, at(12), at(12), "an email that holds mine")
	commit(me, at(13), at(13), "the last")
	commit(me, at(-5), at(14), "rebased into the range")
	run("checkout", "-q", "-b", "side")
	commit(me, at(9), at(15), "on a branch, authored first")
	run("checkout", "-q", "main")
	run("merge", "-q", "--no-ff", "-m", "a merge", "side")
	commit(me, at(7*24+1), at(7*24+1), "after the range")

	got, err := NewCore().AuthoredCommits(context.Background(), repo, from, to)
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, c := range got {
		subjects = append(subjects, c.At.UTC().Format("15:04")+" "+c.Subject)
		if len(c.Hash) != 40 {
			t.Errorf("commit %q has hash %q", c.Subject, c.Hash)
		}
	}
	want := []string{"09:00 on a branch, authored first", "10:00 a\ttab and ü", "13:00 the last"}
	if !slices.Equal(subjects, want) {
		t.Fatalf("the commits are %q\nwant %q", subjects, want)
	}
}

// A repository with no user.email has no commits of the person's, and a
// directory that is not a repository is an error.
func TestAuthoredCommitsWithNoEmailOrNoRepository(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	day := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	got, err := NewCore().AuthoredCommits(context.Background(), repo, day, day.AddDate(0, 0, 1))
	if err != nil || got != nil {
		t.Fatalf("a repository with no user.email answers %v, %v", got, err)
	}
	if _, err := NewCore().AuthoredCommits(context.Background(), filepath.Join(repo, "gone"), day, day.AddDate(0, 0, 1)); err == nil {
		t.Fatal("a directory that does not exist answers no error")
	}
}

// Commits past the most one read takes fail with a sentence a person can
// act on: whose commits, since when, and that a range nearer today reads
// fewer.
func TestAuthoredCommitsPastTheReadLimitSaySo(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	repo, commit, _ := authoredRepo(t)
	for h := range 4 {
		commit("me@example.com", from.Add(time.Duration(h)*time.Hour), from.Add(time.Duration(h)*time.Hour), "a subject long enough to pass the limit")
	}
	core := NewCore()
	core.maxOutputBytes = 100
	_, err := core.AuthoredCommits(context.Background(), repo, from, from.AddDate(0, 0, 1))
	want := "git: your commits in " + repo + " since 2026-11-23 are more than the 100 bytes of git log one read takes; a range nearer today reads fewer"
	if err == nil || err.Error() != want {
		t.Fatalf("the read past its limit answers %v\nwant %s", err, want)
	}
}
