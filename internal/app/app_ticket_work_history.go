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
// (app_forge_history.go): the pull requests that name its key in the
// repositories Work's projects clone, and in their parents with those
// repositories as their head (store.ForgeHistoryForWork), and for each of
// Work's origins whether that history answers for the ticket or the forge
// is searched by the key (SearchTicketWork).

// ticketHistory is what the stored history holds for a ticket's Work.
type ticketHistory struct {
	store.ForgeHistoryWork
	byKey map[string]store.ForgeHistorySource
}

// readTicketHistory reads the stored history of read's ticket for its
// origins.
func (a *App) readTicketHistory(read *ticketWorkRead) (ticketHistory, error) {
	var keys []string
	for _, origin := range read.origins {
		if origin.Error == "" && !slices.Contains(keys, origin.Repo.Identity()) {
			keys = append(keys, origin.Repo.Identity())
		}
	}
	work, err := a.store.ForgeHistoryForWork(read.work.Key, keys)
	if err != nil {
		return ticketHistory{}, err
	}
	h := ticketHistory{ForgeHistoryWork: work, byKey: map[string]store.ForgeHistorySource{}}
	for _, s := range work.Sources {
		h.byKey[s.RepoKey] = s
	}
	return h, nil
}

// sources are the stored sources an origin's answer reads: its own, and
// its parent's once its own read found it a fork. needed is false when one
// of those is not stored yet.
func (h ticketHistory) sources(origin ForgeProject) (have []store.ForgeHistorySource, needed bool) {
	own, ok := h.byKey[origin.Repo.Identity()]
	if !ok {
		return nil, false
	}
	have = append(have, own)
	if own.Parent.RepoKey == "" {
		return have, true
	}
	parent, ok := h.byKey[own.Parent.RepoKey]
	if !ok {
		return have, false
	}
	return append(have, parent), true
}

// catchUpTicketHistory reads again each stored source of Work's origins
// and their parents whose last read is forgeHistoryCatchUp old, one read
// each, at most prListReadConcurrency at once, within ctx and its
// priority; a read of the source already running is joined. A source's
// forge failure is recorded on it (readForgeHistorySource); the error
// returned is the store's. A read ctx ended stores nothing and is no
// error: the stored history answers.
func (a *App) catchUpTicketHistory(ctx context.Context, read *ticketWorkRead) error {
	h, err := a.readTicketHistory(read)
	if err != nil {
		return err
	}
	errs := make([]error, len(h.Sources))
	runBounded(len(h.Sources), prListReadConcurrency, func(i int) {
		errs[i] = a.readForgeHistorySource(ctx, h.Sources[i].ID, forgeHistoryStale)
	})
	if ctx.Err() != nil {
		return nil
	}
	return errors.Join(errs...)
}

// addTicketHistory lists in read the stored history's pull requests of
// the ticket (store.ForgeHistoryForWork), each under the first of Work's
// projects whose origin is its repository, or whose origin heads it in a
// parent, at the time the forge was last asked for it
// (ticketWorkRead.observe), with its head repository, and records each
// origin's own head (answerTicketWork folds a branch by them). It answers
// the origins whose history cannot answer for the ticket, for the
// forge's search, with the origins git could not read, whose search says
// so:
//
//   - an origin with no stored source, or a fork whose parent has none
//     yet;
//   - an origin whose source, or whose parent's, does not cover the
//     ticket (store.ForgeHistorySource.Covers): its read has not reached
//     back to the ticket's creation, or the ticket is older than the
//     window.
//
// An origin one of whose stored sources failed its last read is not
// searched, whatever else is missing: its stored rows stand beside its
// failure, one ForgeProblem per source.
func (a *App) addTicketHistory(read *ticketWorkRead) ([]ForgeProject, error) {
	h, err := a.readTicketHistory(read)
	if err != nil {
		return nil, err
	}
	var search []ForgeProject
	reported := map[int64]bool{}
	windowStart := forgeHistoryWindowStart(a.forgeHistoryNow()).UnixMilli()
	// ownSource and parentSource are the project a source's rows are
	// listed under: by source id for an origin's, and by the parent's id
	// and the head for a parent's.
	ownSource := map[int64]string{}
	parentSource := map[string]string{}
	for _, origin := range read.origins {
		if origin.Error != "" {
			search = append(search, origin)
			continue
		}
		have, needed := h.sources(origin)
		if len(have) > 0 {
			own := have[0]
			if own.OwnHead != "" {
				read.ownHead(origin.ProjectID, own.OwnHead)
			}
			if _, ok := ownSource[own.ID]; !ok {
				ownSource[own.ID] = origin.ProjectID
			}
			if len(have) > 1 && own.OwnHead != "" {
				key := fmt.Sprintf("%d\x00%s", have[1].ID, own.OwnHead)
				if _, ok := parentSource[key]; !ok {
					parentSource[key] = origin.ProjectID
				}
			}
		}
		failed, covered := false, needed
		for _, s := range have {
			covered = covered && s.Covers(read.created, windowStart)
			if s.Failure == "" {
				continue
			}
			failed = true
			if !reported[s.ID] {
				reported[s.ID] = true
				problem, err := ticketHistoryProblem(origin.ProjectID, s)
				if err != nil {
					return nil, err
				}
				read.work.ForgeProblems = append(read.work.ForgeProblems, problem)
			}
		}
		if !failed && !covered {
			search = append(search, origin)
		}
	}
	byID := map[int64]store.ForgeHistorySource{}
	for _, s := range h.Sources {
		byID[s.ID] = s
	}
	for _, row := range h.Rows {
		src := byID[row.SourceID]
		projectID, ok := ownSource[row.SourceID]
		if !ok {
			projectID = parentSource[fmt.Sprintf("%d\x00%s", row.SourceID, row.HeadRepo)]
		}
		ref := gitops.PRReference{Forge: src.Forge, Host: src.Host, Namespace: src.Namespace, Repo: src.Repo, Number: int(row.Number)}
		state := row.State
		if state == TicketPRDraft {
			state = TicketPROpen
		}
		read.observeFound(gitops.PRFound{Ref: ref, URL: row.URL, Title: row.Title, State: state, Draft: row.State == TicketPRDraft,
			Author: gitops.PRListAuthor{Login: row.Author, Name: row.AuthorName}, HeadRefName: row.HeadRef}, projectID, row.SeenAt, row.HeadRepo)
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
