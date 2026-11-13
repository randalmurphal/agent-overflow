package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/forgeapi"
)

// GitLab's reactions (award emoji). The discussions read carries none,
// so one GraphQL read of the merge request's notes, MRNoteReactions,
// reads every note's awards (Note.awardEmoji,
// https://docs.gitlab.com/api/graphql/reference/interfaces/#basenoteinterface).
// It is made when the notes changed, after the viewer's own reaction,
// or once gitlabReactionsMaxAge passed, and answered from memory
// otherwise. Its failure never fails the threads: they carry no
// reactions and the read says so (PRRead.ReactionsUnavailable); the
// failure is logged and not asked again until its retry is due. The
// writes are the REST award emoji endpoints
// (https://docs.gitlab.com/api/emoji_reactions/).

const gitlabNoteReactionsQuery = `query MRNoteReactions($fullPath: ID!, $iid: String!, $after: String) {
  project(fullPath: $fullPath) {
    mergeRequest(iid: $iid) {
      notes(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { id awardEmoji(first: 100) { pageInfo { hasNextPage } nodes { name user { username } } } }
      }
    }
  }
}`

const (
	// gitlabReactionNotesMaxPages bounds the notes pages one read takes:
	// 1000 notes, system notes included. Notes past it carry no reactions
	// and the read says so.
	gitlabReactionNotesMaxPages = 10
	// gitlabReactionsMaxAge is how long a read stands while the notes are
	// unchanged: others' reactions show within it.
	gitlabReactionsMaxAge = 2 * time.Minute
	// gitlabAwardsMaxPages bounds a write's read of one note's awards
	// (100 per page).
	gitlabAwardsMaxPages = 10
)

// gitlabAward is one award emoji on a note.
type gitlabAward struct {
	Name, User string
}

// gitlabNoteReactions is one read: each note's awards by note id, and
// what it could not cover (a sentence, empty when complete).
type gitlabNoteReactions struct {
	stamp   string
	readAt  time.Time
	awards  map[int64][]gitlabAward
	partial string
}

// gitlabReactionMemory is the reaction reads per login and merge
// request, bounded by gitlabCacheMax (the oldest read goes), with the
// read in flight per key (concurrent cold reads share it) and the
// failures waiting for their retry.
//
// Every invalidation of a key (the viewer's reaction write) and every
// clear (Core.ResetForgeCaches) starts a new generation. A read records
// what it learned (store, failed, a host that lacks the field) only in
// the generation it started in, and a read started in an older one is
// not joined, so nothing read before an invalidation or a reset outlives
// it.
type gitlabReactionMemory struct {
	mu       sync.Mutex
	reads    map[string]gitlabNoteReactions
	flights  map[string]*gitlabReactionFlight
	failures map[string]gitlabStateEventsRetry
	// epoch counts clears; gens counts each key's invalidations.
	epoch uint64
	gens  map[string]uint64
}

// gitlabReactionGen is the generation a read started in.
type gitlabReactionGen struct {
	key        string
	epoch, gen uint64
}

type gitlabReactionFlight struct {
	gen    gitlabReactionGen
	done   chan struct{}
	result gitlabNoteReactions
	err    error
}

func (m *gitlabReactionMemory) clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads, m.failures, m.flights, m.gens = nil, nil, nil, nil
	m.epoch++
}

// invalidate drops key's read and starts its next generation, so the
// next read asks the forge and no read in flight is kept.
func (m *gitlabReactionMemory) invalidate(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.reads, key)
	delete(m.flights, key)
	if m.gens == nil {
		m.gens = map[string]uint64{}
	}
	m.gens[key]++
}

func (m *gitlabReactionMemory) generationLocked(key string) gitlabReactionGen {
	return gitlabReactionGen{key: key, epoch: m.epoch, gen: m.gens[key]}
}

// ifCurrent runs record under the memory's lock when gen is still
// current, and reports whether it ran.
func (m *gitlabReactionMemory) ifCurrent(gen gitlabReactionGen, record func()) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generationLocked(gen.key) != gen {
		return false
	}
	record()
	return true
}

// fresh is key's read when it covers stamp and is younger than
// gitlabReactionsMaxAge.
func (m *gitlabReactionMemory) fresh(key, stamp string, now time.Time) (gitlabNoteReactions, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	read, ok := m.reads[key]
	if !ok || read.stamp != stamp || now.Sub(read.readAt) >= gitlabReactionsMaxAge || now.Before(read.readAt) {
		return gitlabNoteReactions{}, false
	}
	return read, true
}

// waiting is key's failure while its retry is not due.
func (m *gitlabReactionMemory) waiting(key string, now time.Time) (gitlabStateEventsRetry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	retry, ok := m.failures[key]
	return retry, ok && now.Before(retry.due)
}

// store keeps read for gen's key while gen is current.
func (m *gitlabReactionMemory) store(gen gitlabReactionGen, read gitlabNoteReactions) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generationLocked(gen.key) != gen {
		return
	}
	key := gen.key
	delete(m.failures, key)
	if m.reads == nil {
		m.reads = map[string]gitlabNoteReactions{}
	}
	if _, ok := m.reads[key]; !ok && len(m.reads) >= gitlabCacheMax {
		oldest := ""
		for k, entry := range m.reads {
			if oldest == "" || entry.readAt.Before(m.reads[oldest].readAt) {
				oldest = k
			}
		}
		delete(m.reads, oldest)
	}
	m.reads[key] = read
}

// failed records the failure of gen's key while gen is current and
// returns its count (one past the recorded count when gen is not).
func (m *gitlabReactionMemory) failed(gen gitlabReactionGen, due func(failures int) time.Time) gitlabStateEventsRetry {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := gen.key
	if m.generationLocked(key) != gen {
		retry := m.failures[key]
		retry.failures++
		retry.due = due(retry.failures)
		return retry
	}
	if m.failures == nil {
		m.failures = map[string]gitlabStateEventsRetry{}
	}
	if _, ok := m.failures[key]; !ok && len(m.failures) >= gitlabCacheMax {
		for k := range m.failures {
			delete(m.failures, k)
			break
		}
	}
	retry := m.failures[key]
	retry.failures++
	retry.due = due(retry.failures)
	m.failures[key] = retry
	return retry
}

// do runs read for key, or joins the one in flight (an invalidation or a
// clear drops it, so it is always of key's current generation); it runs past the caller that started it, each waiter
// bounded by its own context. It answers the generation the read
// started in, which store and failed record against.
func (m *gitlabReactionMemory) do(ctx context.Context, key string, read func(context.Context) (gitlabNoteReactions, error)) (gitlabNoteReactions, gitlabReactionGen, error) {
	m.mu.Lock()
	if m.flights == nil {
		m.flights = map[string]*gitlabReactionFlight{}
	}
	gen := m.generationLocked(key)
	flight := m.flights[key]
	if flight == nil {
		flight = &gitlabReactionFlight{gen: gen, done: make(chan struct{})}
		m.flights[key] = flight
		go func() {
			result, err := read(context.WithoutCancel(ctx))
			m.mu.Lock()
			flight.result, flight.err = result, err
			if m.flights[key] == flight {
				delete(m.flights, key)
			}
			m.mu.Unlock()
			close(flight.done)
		}()
	}
	m.mu.Unlock()
	select {
	case <-flight.done:
		return flight.result, flight.gen, flight.err
	case <-ctx.Done():
		return gitlabNoteReactions{}, flight.gen, ctx.Err()
	}
}

// gitlabNotesStamp names the notes threads hold, so a read is made again
// when one is added or removed.
func gitlabNotesStamp(threads []ReviewThread) string {
	ids := []int64{}
	for _, thread := range threads {
		for _, comment := range thread.Comments {
			ids = append(ids, comment.DatabaseID)
		}
	}
	slices.Sort(ids)
	sum := sha256.New()
	for _, id := range ids {
		sum.Write(strconv.AppendInt(nil, id, 10))
		sum.Write([]byte{','})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// gitlabReactionsKey is the memory key of ref's reactions for the login
// fp: the login, and the merge request's identity (lowercased).
func gitlabReactionsKey(fp string, ref PRReference) string {
	return fp + " " + ref.Identity()
}

// fillReactions sets each comment's reactions and answers the sentence
// for what it could not (empty when it filled them all). The threads
// stand whatever happens.
func (f *gitlabForge) fillReactions(ctx context.Context, client *forgeapi.Client, ref PRReference, threads []ReviewThread) string {
	unavailable := "Reactions are not shown: GitLab's reactions could not be read."
	if _, lacks := f.noNoteAwardEmoji.get(strings.ToLower(ref.Host), f.core.nowFn()); lacks {
		return "Reactions are not shown: this GitLab server does not report them."
	}
	fp, err := client.CredentialFingerprint(ctx)
	if err != nil {
		return unavailable
	}
	key, stamp, now := gitlabReactionsKey(fp, ref), gitlabNotesStamp(threads), f.core.nowFn()
	read, ok := f.reactions.fresh(key, stamp, now)
	if !ok {
		if _, waiting := f.reactions.waiting(key, now); waiting {
			return unavailable
		}
		var gen gitlabReactionGen
		read, gen, err = f.reactions.do(ctx, key, func(ctx context.Context) (gitlabNoteReactions, error) {
			return readGitLabNoteReactions(ctx, client, ref)
		})
		if err != nil {
			if ctx.Err() != nil {
				return unavailable
			}
			if gitlabLacksAwardEmoji(err) {
				f.reactions.ifCurrent(gen, func() { f.noNoteAwardEmoji.put(strings.ToLower(ref.Host), true, f.core.nowFn()) })
				log.Printf("gitlab: %s has no Note.awardEmoji; reactions are not read there for %s: %v", ref.Host, gitlabCacheTTL, err)
				return "Reactions are not shown: this GitLab server does not report them."
			}
			retry := f.reactions.failed(gen, func(failures int) time.Time { return now.Add(f.core.prLookupErrorDelay(failures)) })
			log.Printf("gitlab: reactions of %s!%d on %s unavailable until %s (failure %d): %v", ref.Project(), ref.Number, ref.Host, retry.due.Format(time.RFC3339), retry.failures, err)
			return unavailable
		}
		read.stamp, read.readAt = stamp, now
		f.reactions.store(gen, read)
	}
	viewer := f.viewerLogin(ctx, client, ref.Host)
	for i := range threads {
		for j := range threads[i].Comments {
			comment := &threads[i].Comments[j]
			comment.Reactions = gitlabCommentReactions(read.awards[comment.DatabaseID], viewer)
		}
	}
	return read.partial
}

// gitlabCommentReactions groups a note's awards by name, in the order
// each name was first awarded.
func gitlabCommentReactions(awards []gitlabAward, viewer string) []CommentReaction {
	var out []CommentReaction
	index := map[string]int{}
	for _, award := range awards {
		i, ok := index[award.Name]
		if !ok {
			i = len(out)
			index[award.Name] = i
			out = append(out, CommentReaction{Name: award.Name})
		}
		out[i].Count++
		out[i].Users = append(out[i].Users, award.User)
		if viewer != "" && strings.EqualFold(award.User, viewer) {
			out[i].ViewerReacted = true
		}
	}
	return out
}

// gitlabLacksAwardEmoji is a server whose schema has no Note.awardEmoji.
func gitlabLacksAwardEmoji(err error) bool {
	var gql *forgeapi.GraphQLError
	if !errors.As(err, &gql) {
		return false
	}
	for _, problem := range gql.Problems {
		if strings.Contains(problem.Message, "awardEmoji") && strings.Contains(problem.Message, "doesn't exist") {
			return true
		}
	}
	return false
}

// readGitLabNoteReactions reads the notes' awards, at most
// gitlabReactionNotesMaxPages pages. A note with more awards than one
// page, and the notes past the last page, carry none, and partial says
// so.
func readGitLabNoteReactions(ctx context.Context, client *forgeapi.Client, ref PRReference) (gitlabNoteReactions, error) {
	out := gitlabNoteReactions{awards: map[int64][]gitlabAward{}}
	vars := map[string]any{"fullPath": ref.Project(), "iid": strconv.Itoa(ref.Number)}
	overflow := false
	for page := range gitlabReactionNotesMaxPages {
		var raw struct {
			Project *struct {
				MergeRequest *struct {
					Notes struct {
						PageInfo githubPageInfo `json:"pageInfo"`
						Nodes    []struct {
							ID         string `json:"id"`
							AwardEmoji struct {
								PageInfo githubPageInfo `json:"pageInfo"`
								Nodes    []struct {
									Name string `json:"name"`
									User *struct {
										Username string `json:"username"`
									} `json:"user"`
								} `json:"nodes"`
							} `json:"awardEmoji"`
						} `json:"nodes"`
					} `json:"notes"`
				} `json:"mergeRequest"`
			} `json:"project"`
		}
		if _, err := client.GraphQL(ctx, "MRNoteReactions", gitlabNoteReactionsQuery, vars, &raw); err != nil {
			return gitlabNoteReactions{}, err
		}
		if raw.Project == nil || raw.Project.MergeRequest == nil {
			return gitlabNoteReactions{}, fmt.Errorf("GitLab answered no merge request %s!%d", ref.Project(), ref.Number)
		}
		notes := raw.Project.MergeRequest.Notes
		for _, node := range notes.Nodes {
			id, err := gitlabGIDNumber(node.ID)
			if err != nil {
				return gitlabNoteReactions{}, err
			}
			if node.AwardEmoji.PageInfo.HasNextPage {
				overflow = true
				continue
			}
			for _, award := range node.AwardEmoji.Nodes {
				user := ""
				if award.User != nil {
					user = award.User.Username
				}
				out.awards[id] = append(out.awards[id], gitlabAward{Name: award.Name, User: user})
			}
		}
		if !notes.PageInfo.HasNextPage {
			break
		}
		if notes.PageInfo.EndCursor == "" {
			return gitlabNoteReactions{}, errors.New("GitLab notes have another page but no cursor")
		}
		if page == gitlabReactionNotesMaxPages-1 {
			out.partial = fmt.Sprintf("Reactions are shown on the first %d comments only.", gitlabReactionNotesMaxPages*gitlabPageSize)
			break
		}
		vars["after"] = notes.PageInfo.EndCursor
	}
	if overflow && out.partial == "" {
		out.partial = "Reactions are not shown on comments with more than 100."
	}
	return out, nil
}

// gitlabGIDNumber is the number at the end of a GitLab global id
// ("gid://gitlab/DiffNote/42").
func gitlabGIDNumber(gid string) (int64, error) {
	i := strings.LastIndexByte(gid, '/')
	n, err := strconv.ParseInt(gid[i+1:], 10, 64)
	if err != nil || n <= 0 || !strings.HasPrefix(gid, "gid://gitlab/") {
		return 0, fmt.Errorf("GitLab id %q is not a global id", gid)
	}
	return n, nil
}

// gitlabAwardRaw is one award emoji as the REST API answers it.
type gitlabAwardRaw struct {
	ID   int64           `json:"id"`
	Name string          `json:"name"`
	User gitlabAuthorRaw `json:"user"`
}

// SetCommentReaction reads the note's awards under this merge request
// (a note of another is GitLab's 404, refused as *CommentNotOnPRError)
// to find the viewer's, then posts or deletes it when it is not already
// as asked. The removal names the award the read found, never one from
// the caller.
func (f *gitlabForge) SetCommentReaction(ctx context.Context, ref PRReference, target CommentTarget, name string, reacted bool) error {
	noteID, err := strconv.ParseInt(target.CommentID, 10, 64)
	if err != nil || noteID <= 0 {
		return fmt.Errorf("GitLab note id %q is not a note id", target.CommentID)
	}
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	viewer, err := f.readViewerLogin(ctx, client, ref.Host)
	if err != nil {
		return err
	}
	if fp, err := client.CredentialFingerprint(ctx); err == nil {
		defer f.reactions.invalidate(gitlabReactionsKey(fp, ref))
	}
	path := gitlabMRPath(ref) + "/notes/" + strconv.FormatInt(noteID, 10) + "/award_emoji"
	var mine *gitlabAwardRaw
	pages, more := 0, false
	request := forgeapi.Request{Path: path, Query: url.Values{"per_page": {strconv.Itoa(gitlabPageSize)}}}
	page := 1
	err = client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var awards []gitlabAwardRaw
		if err := json.Unmarshal(resp.Body, &awards); err != nil {
			return false, fmt.Errorf("GitLab award emoji: decode response: %w", err)
		}
		for i := range awards {
			if awards[i].Name == name && strings.EqualFold(awards[i].User.Username, viewer) {
				mine = &awards[i]
			}
		}
		pages++
		next := advanceGitLabPage(resp, &page)
		more = next && pages >= gitlabAwardsMaxPages
		return mine == nil && next && pages < gitlabAwardsMaxPages, nil
	})
	if errors.Is(err, forgeapi.ErrNotFound) {
		return &CommentNotOnPRError{CommentID: target.CommentID}
	}
	if err != nil {
		return err
	}
	switch {
	case reacted && mine != nil, !reacted && mine == nil && !more:
		return nil
	case !reacted && mine == nil:
		return fmt.Errorf("your %s reaction is not among the first %d on this comment", name, gitlabAwardsMaxPages*gitlabPageSize)
	case reacted:
		var created gitlabAwardRaw
		_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: path, Body: map[string]string{"name": name}}, &created)
		if gitlabAlreadyAwarded(err) {
			return nil
		}
		if err != nil {
			return writeOutcome(err)
		}
		if created.Name != name {
			return &UnconfirmedWriteError{Err: fmt.Errorf("GitLab answered the %s reaction with %q", name, created.Name)}
		}
		return nil
	}
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodDelete, Path: path + "/" + strconv.FormatInt(mine.ID, 10)}, nil)
	if errors.Is(err, forgeapi.ErrNotFound) {
		return nil // removed in between: the reaction is gone, as asked
	}
	return writeOutcome(err)
}

// gitlabAlreadyAwarded is GitLab's refusal of an award the user already
// gave: a 404 whose message says the name is taken.
func gitlabAlreadyAwarded(err error) bool {
	var status *forgeapi.StatusError
	return errors.As(err, &status) && status.Status == http.StatusNotFound && strings.Contains(strings.ToLower(status.Body), "has already been taken")
}
