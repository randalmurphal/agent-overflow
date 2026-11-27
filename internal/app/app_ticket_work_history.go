package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// A ticket's Work from the stored forge pull request history
// (app_forge_history.go): the pull requests that name its key, and for
// each of Work's origins whether that history answers for the ticket or
// the forge is searched by the key (SearchTicketWork).

// ticketHistorySources are the stored sources an origin's answer reads:
// its own, and its parent's when it is a fork, which is the parent's
// origin source when a project clones the parent too.
func ticketHistorySources(origin ForgeProject, stored []store.ForgeHistorySource) (own store.ForgeHistorySource, parent *store.ForgeHistorySource, ok bool) {
	find := func(repoKey, keepHead string) (store.ForgeHistorySource, bool) {
		at := slices.IndexFunc(stored, func(s store.ForgeHistorySource) bool { return s.RepoKey == repoKey && s.KeepHead == keepHead })
		if at < 0 {
			return store.ForgeHistorySource{}, false
		}
		return stored[at], true
	}
	own, ok = find(origin.Repo.Identity(), "")
	if !ok {
		return own, nil, false
	}
	ref, fork := forgeHistoryParent(own)
	if !fork {
		return own, nil, true
	}
	if p, found := find(ref.RepoKey, ""); found {
		return own, &p, true
	}
	p, found := find(ref.RepoKey, ref.KeepHead)
	if !found {
		return own, nil, false
	}
	return own, &p, true
}

// catchUpTicketHistory reads again each stored source of Work's origins
// (ticketHistorySources) whose last read is forgeHistoryCatchUp old, at
// most prListReadConcurrency at once, within ctx and its priority. A
// source's forge failure is recorded on it (readForgeHistorySource); the
// error returned is the store's. A read ctx ended stores nothing and is
// no error: the stored history answers.
func (a *App) catchUpTicketHistory(ctx context.Context, read *ticketWorkRead) error {
	stored, err := a.store.ListForgeHistorySources()
	if err != nil {
		return err
	}
	var refs []store.ForgeHistoryRef
	add := func(s store.ForgeHistorySource) {
		if !slices.ContainsFunc(refs, func(r store.ForgeHistoryRef) bool {
			return forgeHistoryLockKey(r) == forgeHistoryLockKey(s.ForgeHistoryRef)
		}) {
			refs = append(refs, s.ForgeHistoryRef)
		}
	}
	for _, origin := range read.origins {
		if origin.Error != "" {
			continue
		}
		own, parent, _ := ticketHistorySources(origin, stored)
		if own.ID != 0 {
			add(own)
		}
		if parent != nil {
			add(*parent)
		}
	}
	errs := make([]error, len(refs))
	runBounded(len(refs), prListReadConcurrency, func(i int) {
		errs[i] = a.readForgeHistorySource(ctx, refs[i], forgeHistoryCatchUp)
	})
	if ctx.Err() != nil {
		return nil
	}
	return errors.Join(errs...)
}

// addTicketHistory adds to read the stored history's pull requests that
// name the key, each under the first of Work's projects whose origin is
// its repository, or for a fork's parent the fork's, and folds each one's
// head into the branch of that name of every project whose origin is its
// head repository. It answers the origins whose history cannot answer
// for the ticket, for the forge's search, with the origins git could not
// read, whose search says so:
//
//   - an origin with no stored source, or a fork whose parent has none
//     yet;
//   - an origin whose source, or whose parent's, does not cover the
//     ticket (store.ForgeHistorySource.Covers): its read has not reached
//     back to the ticket's creation, or the ticket is older than the
//     window.
//
// An origin whose source, or whose parent's, failed its last read is not
// searched: its stored rows stand beside its failure, one ForgeProblem
// per source.
func (a *App) addTicketHistory(read *ticketWorkRead) ([]ForgeProject, error) {
	stored, err := a.store.ListForgeHistorySources()
	if err != nil {
		return nil, err
	}
	rows, err := a.store.ForgeHistoryByKey(read.work.Key)
	if err != nil {
		return nil, err
	}
	// heads are the origins' head identities, by project.
	heads := map[string]string{}
	var search []ForgeProject
	failed := map[int64]bool{}
	windowStart := forgeHistoryWindowStart(a.forgeHistoryNow()).UnixMilli()
	for _, origin := range read.origins {
		if origin.Error != "" {
			search = append(search, origin)
			continue
		}
		own, parent, ok := ticketHistorySources(origin, stored)
		if own.OwnHead != "" {
			heads[origin.ProjectID] = own.OwnHead
		}
		if !ok {
			search = append(search, origin)
			continue
		}
		sources := []store.ForgeHistorySource{own}
		if parent != nil {
			sources = append(sources, *parent)
		}
		answered, anyFailed := true, false
		for _, s := range sources {
			if s.Failure != "" {
				anyFailed = true
				if !failed[s.ID] {
					failed[s.ID] = true
					problem, err := ticketHistoryProblem(origin.ProjectID, s)
					if err != nil {
						return nil, err
					}
					read.work.ForgeProblems = append(read.work.ForgeProblems, problem)
				}
			}
			answered = answered && s.Covers(read.created, windowStart)
		}
		if !answered && !anyFailed {
			search = append(search, origin)
		}
	}
	for _, row := range rows {
		ref := gitops.PRReference{Forge: row.Source.Forge, Host: row.Source.Host, Namespace: row.Source.Namespace, Repo: row.Source.Repo, Number: int(row.Number)}
		projectID := ""
		for _, origin := range read.origins {
			if origin.Error != "" {
				continue
			}
			if row.Source.KeepHead == "" && origin.Repo.Identity() == row.Source.RepoKey ||
				row.Source.KeepHead != "" && heads[origin.ProjectID] == row.Source.KeepHead {
				projectID = origin.ProjectID
				break
			}
		}
		state := row.State
		if state == TicketPRDraft {
			state = TicketPROpen
		}
		found := gitops.PRFound{Ref: ref, URL: row.URL, Title: row.Title, State: state, Draft: row.State == TicketPRDraft,
			Author: gitops.PRListAuthor{Login: row.Author, Name: row.AuthorName}, HeadRefName: row.HeadRef}
		if !read.addFound(found, projectID) || row.HeadRepo == "" {
			continue
		}
		for _, b := range read.branches {
			if b.Name != row.HeadRef || heads[b.ProjectID] != row.HeadRepo {
				continue
			}
			head := ticketHead{b.ProjectID, b.Name}
			if read.heads == nil {
				read.heads = map[ticketHead][]string{}
			}
			if !slices.Contains(read.heads[head], ref.Identity()) {
				read.heads[head] = append(read.heads[head], ref.Identity())
			}
		}
	}
	return search, nil
}

// ticketHistoryProblem is source s's failed read as Work shows it, under
// projectID.
func ticketHistoryProblem(projectID string, s store.ForgeHistorySource) (TicketForgeProblem, error) {
	var detail forgeHistoryFailureDetail
	if len(s.FailureDetail) > 0 {
		if err := json.Unmarshal(s.FailureDetail, &detail); err != nil {
			return TicketForgeProblem{}, fmt.Errorf("read the failure of the pull request history of %s: %w", s.RepoKey, err)
		}
	}
	return TicketForgeProblem{
		ProjectID: projectID, Repo: forgeHistoryRepo(s.ForgeHistoryRef), Error: s.Failure, ErrorKind: s.FailureKind,
		ErrorDetails: detail.ErrorDetails, Reserve: detail.Reserve, ResumeAt: detail.ResumeAt, History: true, FailedAt: s.FailedAt,
	}, nil
}
