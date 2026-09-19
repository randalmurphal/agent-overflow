package threadtools

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"
)

// thread_search: one tool for finding threads and for listing them. Rows
// have the same shape either way; a query adds the matched item and a
// snippet.
//
// Each computer searches its own database, so a multi-computer call runs
// on each of them concurrently under SearchTimeout and returns rows
// grouped per computer in that computer's own order. FTS ranks from
// separate indexes are not comparable, so there is no merged ordering to
// offer, and limit applies per computer. A computer that is offline or too
// old contributes an errors row and the rest of the call still succeeds.

type searchArgs struct {
	Query       string   `json:"query"`
	Computers   []string `json:"computers"`
	ThreadID    string   `json:"thread_id"`
	Kind        string   `json:"kind"`
	ProjectID   string   `json:"project_id"`
	Provider    string   `json:"provider"`
	State       string   `json:"state"`
	Archived    *bool    `json:"archived"`
	SpawnedByMe bool     `json:"spawned_by_me"`
	Since       string   `json:"since"`
	Limit       int      `json:"limit"`
	Cursor      string   `json:"cursor"`
}

type searchRow struct {
	ComputerID   string `json:"computer_id,omitempty"`
	Computer     string `json:"computer,omitempty"`
	ThreadID     string `json:"thread_id"`
	Title        string `json:"title"`
	ProjectID    string `json:"project_id,omitempty"`
	Project      string `json:"project,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	State        string `json:"state"`
	LastActivity string `json:"last_activity,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Group        string `json:"group,omitempty"`
	Pin          string `json:"pin,omitempty"`
	Archived     bool   `json:"archived"`
	Unread       bool   `json:"unread"`
	ItemID       string `json:"item_id,omitempty"`
	Snippet      string `json:"snippet,omitempty"`
}

type searchGroup struct {
	ComputerID string      `json:"computer_id,omitempty"`
	Computer   string      `json:"computer,omitempty"`
	Indexing   bool        `json:"indexing,omitempty"`
	More       bool        `json:"more,omitempty"`
	Rows       []searchRow `json:"rows"`
}

// The two result shapes. A computer with no pairings answers with rows
// and nothing about computers at all; a paired one answers with the rows
// grouped per computer. They are separate types so neither can leak the
// other's fields as zero values.
type searchSolo struct {
	Rows     []searchRow `json:"rows"`
	Indexing bool        `json:"indexing,omitempty"`
	More     bool        `json:"more,omitempty"`
	Cursor   string      `json:"cursor,omitempty"`
	Note     string      `json:"note,omitempty"`
}

type searchGrouped struct {
	Computers []searchGroup `json:"computers"`
	Errors    []errorRow    `json:"errors,omitempty"`
	Cursor    string        `json:"cursor,omitempty"`
	Note      string        `json:"note,omitempty"`
}

// searchAnswerShape reads either shape, which is what a peer's reply
// needs: the destination answered in ITS shape, not the caller's.
type searchAnswerShape struct {
	Rows      []searchRow   `json:"rows"`
	Indexing  bool          `json:"indexing"`
	More      bool          `json:"more"`
	Computers []searchGroup `json:"computers"`
}

func (c *session) search(ctx context.Context, raw json.RawMessage) (any, error) {
	var args searchArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	query, err := c.searchQuery(args)
	if err != nil {
		return nil, err
	}
	page, err := decodeCursor(args.Cursor, cursorSearch)
	if err != nil {
		return nil, err
	}
	targets, err := c.searchTargets(ctx, args, &query)
	if err != nil {
		return nil, err
	}

	groups, failures := c.runSearch(ctx, targets, query, page.Offsets)
	return c.searchResult(groups, failures, page.Offsets)
}

// searchQuery validates the filters and applies the defaults the spec
// names: archived included with a query and excluded without one, and a
// different row count for each.
func (c *session) searchQuery(args searchArgs) (SearchQuery, error) {
	query := SearchQuery{
		Query:     trim(args.Query),
		Kind:      trim(args.Kind),
		ProjectID: trim(args.ProjectID),
		Provider:  trim(args.Provider),
		State:     trim(args.State),
		Archived:  args.Archived,
		Limit:     args.Limit,
	}
	if query.Kind != "" && !slices.Contains([]string{"user", "assistant", "tool", "title"}, query.Kind) {
		return SearchQuery{}, invalidf("kind must be one of user, assistant, tool or title.")
	}
	if query.State != "" && !slices.Contains(AllStates, query.State) {
		return SearchQuery{}, invalidf("state must be one of %s.", joinNames(AllStates))
	}
	if args.Since != "" {
		since, err := parseTimestamp(args.Since, "since")
		if err != nil {
			return SearchQuery{}, err
		}
		query.SinceUnixMs = since
	}
	switch {
	case query.Limit == 0 && query.Query != "":
		query.Limit = DefaultSearchLimit
	case query.Limit == 0:
		query.Limit = DefaultListLimit
	case query.Limit < 0 || query.Limit > MaxSearchLimit:
		return SearchQuery{}, invalidf("limit must be between 1 and %d.", MaxSearchLimit)
	}
	if args.SpawnedByMe {
		query.SpawnedBy = c.caller.ThreadID
	}
	if query.Archived == nil && query.Query == "" {
		// A listing is the sidebar's answer, which leaves archived threads
		// out; a query still finds them because the text is the point.
		excluded := false
		query.Archived = &excluded
	}
	return query, nil
}

// searchTargets decides which computers this call covers, and resolves a
// thread_id filter to the one computer that holds it.
func (c *session) searchTargets(ctx context.Context, args searchArgs, query *SearchQuery) ([]Computer, error) {
	if ref := trim(args.ThreadID); ref != "" {
		target, err := c.resolve(ctx, ref, "")
		if err != nil {
			return nil, err
		}
		query.ThreadID = target.ThreadID
		computer, _, _ := c.computerByID(target.ComputerID)
		return []Computer{computer}, nil
	}
	named, err := c.namedComputers(args.Computers)
	if err != nil {
		return nil, err
	}
	return named, nil
}

// namedComputers turns the computers filter into a target list. An empty
// filter covers this computer and every paired one; ["local"] covers this
// one alone.
func (c *session) namedComputers(names []string) ([]Computer, error) {
	if len(names) == 0 {
		return append([]Computer{c.self()}, c.computers...), nil
	}
	targets := make([]Computer, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		computer, _, ok := c.computerByID(trim(name))
		if !ok {
			return nil, publicf(CodeInvalidRequest, "There is no computer %q. Use \"local\" for this computer, or an id from thread_options: %s.", name, computerList(c.computers))
		}
		if _, duplicate := seen[computer.ID]; duplicate {
			continue
		}
		seen[computer.ID] = struct{}{}
		targets = append(targets, computer)
	}
	return targets, nil
}

type searchAnswer struct {
	computer Computer
	local    bool
	group    searchGroup
	err      error
}

func (c *session) runSearch(ctx context.Context, targets []Computer, query SearchQuery, offsets map[string]int) ([]searchGroup, []errorRow) {
	bounded, cancel := context.WithTimeout(ctx, SearchTimeout)
	defer cancel()

	answers := make([]searchAnswer, len(targets))
	var wg sync.WaitGroup
	for index, computer := range targets {
		wg.Add(1)
		go func(slot int, computer Computer) {
			defer wg.Done()
			_, local, _ := c.computerByID(computer.ID)
			answers[slot] = c.searchOne(bounded, computer, local, query, offsets[offsetKey(computer, local)])
		}(index, computer)
	}
	wg.Wait()

	groups := make([]searchGroup, 0, len(answers))
	var failures []errorRow
	for _, answer := range answers {
		if answer.err != nil {
			failures = append(failures, newErrorRow(answer.computer, answer.err))
			continue
		}
		groups = append(groups, answer.group)
	}
	return groups, failures
}

// offsetKey is how a cursor names one computer's page. The local computer
// keys on the empty string so a cursor this computer minted for a peer
// reads as that peer's own local page.
func offsetKey(computer Computer, local bool) string {
	if local {
		return ""
	}
	return computer.ID
}

func (c *session) searchOne(ctx context.Context, computer Computer, local bool, query SearchQuery, offset int) searchAnswer {
	if local {
		query.Offset = offset
		page, err := c.app.SearchThreads(ctx, query)
		if err != nil {
			return searchAnswer{computer: computer, local: true, err: err}
		}
		return searchAnswer{computer: computer, local: true, group: c.searchGroup(computer, page)}
	}
	peer, err := c.app.Peer(ctx, computer.ID)
	if err != nil {
		return searchAnswer{computer: computer, err: err}
	}
	args, err := peerSearchArgs(query, offset)
	if err != nil {
		return searchAnswer{computer: computer, err: err}
	}
	raw, err := peer.Query(ctx, "thread_search", args)
	if err != nil {
		return searchAnswer{computer: computer, err: err}
	}
	var result searchAnswerShape
	if err := peerResult(raw, &result); err != nil {
		return searchAnswer{computer: computer, err: err}
	}
	return searchAnswer{computer: computer, group: c.stampGroup(computer, result)}
}

// peerSearchArgs rebuilds the call for one destination. computers is
// always ["local"] so the destination answers about itself and never fans
// out again, and the page offset rides an ordinary cursor because offsets
// are not a tool parameter.
func peerSearchArgs(query SearchQuery, offset int) (json.RawMessage, error) {
	args := map[string]any{"computers": []string{"local"}, "limit": query.Limit}
	if query.Query != "" {
		args["query"] = query.Query
	}
	if query.ThreadID != "" {
		args["thread_id"] = query.ThreadID
	}
	for key, value := range map[string]string{"kind": query.Kind, "project_id": query.ProjectID, "provider": query.Provider, "state": query.State} {
		if value != "" {
			args[key] = value
		}
	}
	if query.Archived != nil {
		args["archived"] = *query.Archived
	}
	if query.SpawnedBy != "" {
		args["spawned_by_me"] = true
	}
	if query.SinceUnixMs != 0 {
		args["since"] = unixMsToRFC3339(query.SinceUnixMs)
	}
	if offset > 0 {
		encoded, err := encodeCursor(cursor{Kind: cursorSearch, Offsets: map[string]int{"": offset}})
		if err != nil {
			return nil, err
		}
		args["cursor"] = encoded
	}
	return json.Marshal(args)
}

func (c *session) searchGroup(computer Computer, page SearchPage) searchGroup {
	id, name := c.stamp(computer)
	rows := make([]searchRow, 0, len(page.Rows))
	for _, hit := range page.Rows {
		rows = append(rows, c.row(computer, hit))
	}
	return searchGroup{ComputerID: id, Computer: name, Indexing: page.Indexing, More: page.More, Rows: rows}
}

// stampGroup re-stamps a peer's answer. The destination answered in its
// own shape and does not know what this computer calls it, so the rows
// carry no computer fields until they are stamped here.
func (c *session) stampGroup(computer Computer, result searchAnswerShape) searchGroup {
	id, name := c.stamp(computer)
	group := searchGroup{ComputerID: id, Computer: name, Indexing: result.Indexing, More: result.More, Rows: result.Rows}
	if len(result.Computers) > 0 {
		inner := result.Computers[0]
		group.Indexing, group.More, group.Rows = inner.Indexing, inner.More, inner.Rows
	}
	for index := range group.Rows {
		group.Rows[index].ComputerID, group.Rows[index].Computer = id, name
	}
	if group.Rows == nil {
		group.Rows = []searchRow{}
	}
	return group
}

func (c *session) row(computer Computer, hit Hit) searchRow {
	id, name := c.stamp(computer)
	thread := hit.Thread
	return searchRow{
		ComputerID:   id,
		Computer:     name,
		ThreadID:     thread.ID,
		Title:        thread.Title,
		ProjectID:    thread.ProjectID,
		Project:      thread.Project,
		Provider:     thread.Provider,
		Model:        thread.Model,
		State:        State(thread, hit.Live),
		LastActivity: unixMsToRFC3339(thread.LastActivity),
		Branch:       thread.Branch,
		Group:        thread.Group,
		Pin:          thread.Pin,
		Archived:     thread.Archived,
		Unread:       thread.Unread,
		ItemID:       hit.ItemID,
		Snippet:      hit.Snippet,
	}
}

func (c *session) searchResult(groups []searchGroup, failures []errorRow, offsets map[string]int) (any, error) {
	next := make(map[string]int, len(groups))
	more := false
	for _, group := range groups {
		key := group.ComputerID
		if !c.paired() || key == c.caller.ComputerID {
			key = ""
		}
		if group.More {
			next[key] = offsets[key] + len(group.Rows)
			more = true
		}
	}
	encoded := ""
	if more {
		value, err := encodeCursor(cursor{Kind: cursorSearch, Offsets: next})
		if err != nil {
			return nil, err
		}
		encoded = value
	}
	note := ""
	if more {
		note = "More rows exist. Pass cursor back unchanged to continue."
	}
	if !c.paired() {
		solo := searchSolo{Rows: []searchRow{}, Cursor: encoded, Note: note}
		if len(groups) == 1 {
			solo.Rows, solo.Indexing, solo.More = groups[0].Rows, groups[0].Indexing, groups[0].More
		}
		if solo.Indexing {
			solo.Note = appendNote(solo.Note, "This computer is still building its search index; these results cover what is indexed so far.")
		}
		return solo, nil
	}
	grouped := searchGrouped{Computers: groups, Errors: failures, Cursor: encoded, Note: note}
	if grouped.Computers == nil {
		grouped.Computers = []searchGroup{}
	}
	for _, group := range groups {
		if group.Indexing {
			grouped.Note = appendNote(grouped.Note, "A computer is still building its search index; its rows cover what is indexed so far.")
			break
		}
	}
	if len(failures) > 0 {
		grouped.Note = appendNote(grouped.Note, "One or more computers did not answer; their threads are not in this result.")
	}
	return grouped, nil
}

func appendNote(note, text string) string {
	if note == "" {
		return text
	}
	return note + " " + text
}

func unixMsToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
