package git

import (
	"context"
	"time"
)

// The pull request history of a repository (docs/architecture/forge-transport.md#pull-request-history):
// each pull request updated inside a window, with who approved and
// commented on it, read a bounded part at a time newest first and then
// kept current. The reader owns its walk, which the caller stores
// whole and hands back to the next read.

// PRHistoryPages bounds how many pages one read of a repository makes of
// each of its streams: GitHub's pull request list (pages of
// githubHistoryPageSize), GitLab's merge request list and its approved
// and commented events feeds (pages of 100 each).
const PRHistoryPages = 10

// PRHistoryInput is what a read starts from.
type PRHistoryInput struct {
	// Walk is the walk the last read returned; empty before the first.
	Walk []byte
	// WindowStart is the oldest update the history keeps.
	WindowStart time.Time
	// Known reports which of numbers the caller's store holds of this
	// repository, so a GitLab event that names a merge request neither it
	// nor the read holds fetches that merge request.
	Known func(numbers []int64) (map[int64]bool, error)
}

// PRHistory is what a read found.
type PRHistory struct {
	// OwnHead is the repository's head identity, as a pull request's
	// HeadRepo names a head in it: GitHub's repository node id, GitLab's
	// project id.
	OwnHead string
	// Parent is the repository the forge says it is a fork of; nil when
	// it is none.
	Parent *RepoReference
	PRs    []PRHistoryPR
	// Acts are an events feed's approvals and comments of pull requests
	// not in PRs (GitLab), to add to the stored ones.
	Acts []PRHistoryAct
	// Walk is the read's walk; nil when the read moved nothing.
	Walk []byte
	// Reach is the forge time back to which every update is read; zero
	// is back to the window's start.
	Reach time.Time
	// Walked reports that the first full read is done.
	Walked bool
	// Behind reports that a stream's read on from the updates it last
	// read (GitHub's from the newest down to them, GitLab's from them up
	// to the newest) used the read's pages before it reached them: more
	// changed since the last read than one read holds. Reach then holds
	// for nothing, and the caller reads again at once, which reads on
	// from where this one stopped.
	Behind bool
}

// PRHistoryPR is one pull request. State is open, draft, merged or
// closed. HeadRepo is its head repository's identity (PRHistory.OwnHead
// of that repository), empty when the forge no longer has it.
type PRHistoryPR struct {
	Number                               int64
	URL, Title, HeadRef, HeadRepo, State string
	Author                               PRListAuthor
	Created, Updated, Merged, Closed     time.Time
	// Acts are its approvals and comments, each person's earliest of each
	// kind; ActsWhole when they are all it has.
	Acts      []PRHistoryAct
	ActsWhole bool
}

// PRHistoryAct is one person's approval or comment on pull request
// Number.
type PRHistoryAct struct {
	Number   int64
	Approved bool
	Person   PRListAuthor
	At       time.Time
}

// ReadPRHistory reads the next part of repo's pull request history.
// A read that fails answers what it read before the failure, with the
// walk moved to it, beside its error.
func (c *Core) ReadPRHistory(ctx context.Context, repo RepoReference, in PRHistoryInput) (PRHistory, error) {
	if err := repo.Validate(); err != nil {
		return PRHistory{}, err
	}
	return c.ForgeByID(repo.Forge).ReadPRHistory(ctx, repo, in)
}

func (nullForge) ReadPRHistory(context.Context, RepoReference, PRHistoryInput) (PRHistory, error) {
	return PRHistory{}, ErrUnsupportedForge
}

// earliestActs keeps each person's earliest act of each kind.
func earliestActs(acts []PRHistoryAct) []PRHistoryAct {
	type key struct {
		number   int64
		approved bool
		login    string
	}
	at := map[key]int{}
	var out []PRHistoryAct
	for _, a := range acts {
		k := key{a.Number, a.Approved, a.Person.Login}
		if i, ok := at[k]; ok {
			if a.At.Before(out[i].At) {
				out[i] = a
			}
			continue
		}
		at[k] = len(out)
		out = append(out, a)
	}
	return out
}

// unixMilli is t in Unix ms, 0 for the zero time.
func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// fromUnixMilli is ms as a time, the zero time for 0.
func fromUnixMilli(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
