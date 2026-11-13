package git

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/forgeapi"
)

// GitLab's management Details read and the state events. The read is
// one GraphQL request beside the detail read (PRManage): reviewers with
// their review state, assignees, labels with their ids and colors, and
// the viewer's userPermissions. What the server's schema has decides
// which controls exist and which document is sent: the re-review
// mutation, mergeRequestUpdate's overrideRequestedChanges (with the
// mergeabilityChecks the read then selects, PRManageChecks), and the
// operationMode of the reviewer, assignee and label mutations. The
// schema is probed once per host per gitlabCacheTTL in a document of its
// own (PRManageSchema), its answer cached either way, so a warm read
// carries no introspection and no document names a field an older
// server refuses. The writes are gitlab_manage_write.go.

const gitlabManageSelection = `userPermissions { updateMergeRequest adminMergeRequest }
      reviewers(first: 100) { nodes { id username name mergeRequestInteraction { reviewState } } }
      assignees(first: 100) { nodes { id username name } }
      labels(first: 100) { nodes { id title color } }`

const gitlabManageSchemaQuery = `query PRManageSchema {
  rereview: __type(name: "MergeRequestReviewerRereviewInput") { name }
  updateInput: __type(name: "MergeRequestUpdateInput") { inputFields { name } }
  setReviewers: __type(name: "MergeRequestSetReviewersInput") { inputFields { name } }
  setAssignees: __type(name: "MergeRequestSetAssigneesInput") { inputFields { name } }
  setLabels: __type(name: "MergeRequestSetLabelsInput") { inputFields { name } }
}`

const gitlabManageQuery = `query PRManage($fullPath: ID!, $iid: String!) {
  project(fullPath: $fullPath) {
    mergeRequest(iid: $iid) {
      ` + gitlabManageSelection + `
    }
  }
}`

const gitlabManageChecksQuery = `query PRManageChecks($fullPath: ID!, $iid: String!) {
  project(fullPath: $fullPath) {
    mergeRequest(iid: $iid) {
      ` + gitlabManageSelection + `
      mergeabilityChecks { identifier status }
    }
  }
}`

// gitlabSchema is what a host's GraphQL schema has of the management
// surface.
type gitlabSchema struct {
	// rereview is mergeRequestReviewerRereview; override is
	// mergeRequestUpdate's overrideRequestedChanges, with which the read
	// selects mergeabilityChecks.
	rereview, override bool
	// setReviewers, setAssignees and setLabels are the set mutations
	// taking an operationMode, which the people and label writes need.
	setReviewers, setAssignees, setLabels bool
}

type gitlabInputField struct {
	Name string `json:"name"`
}

type gitlabInputType struct {
	InputFields []gitlabInputField `json:"inputFields"`
}

func (t *gitlabInputType) has(field string) bool {
	return t != nil && slices.Contains(t.InputFields, gitlabInputField{Name: field})
}

// manageSchema is the host's schema answer: cached, else probed and
// cached (an answer either way; a failed probe is not cached). Concurrent
// cold reads of one host share one probe, which runs past the caller that
// started it; each waits for it bounded by its own context.
func (f *gitlabForge) manageSchema(ctx context.Context, client *forgeapi.Client, host string) (gitlabSchema, error) {
	if known, ok := f.schemas.get(host, f.core.nowFn()); ok {
		return known, nil
	}
	return f.probes.do(ctx, host, func(ctx context.Context) (gitlabSchema, error) {
		return f.probeSchema(ctx, client, host)
	})
}

// gitlabSchemaProbes is the schema probe in flight per host.
type gitlabSchemaProbes struct {
	mu      sync.Mutex
	flights map[string]*gitlabSchemaProbe
}

type gitlabSchemaProbe struct {
	done    chan struct{}
	schema  gitlabSchema
	err     error
	waiters int
}

// do answers host's probe: the one in flight, else one probe starts on
// ctx's values without its cancellation. A waiter whose context ends
// returns its error; the probe goes on for the others.
func (p *gitlabSchemaProbes) do(ctx context.Context, host string, probe func(context.Context) (gitlabSchema, error)) (gitlabSchema, error) {
	if err := ctx.Err(); err != nil {
		return gitlabSchema{}, err
	}
	p.mu.Lock()
	if p.flights == nil {
		p.flights = map[string]*gitlabSchemaProbe{}
	}
	flight := p.flights[host]
	if flight == nil {
		flight = &gitlabSchemaProbe{done: make(chan struct{})}
		p.flights[host] = flight
		go func() {
			schema, err := probe(context.WithoutCancel(ctx))
			p.mu.Lock()
			flight.schema, flight.err = schema, err
			delete(p.flights, host)
			p.mu.Unlock()
			close(flight.done)
		}()
	}
	flight.waiters++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		flight.waiters--
		p.mu.Unlock()
	}()
	select {
	case <-flight.done:
		return flight.schema, flight.err
	case <-ctx.Done():
		return gitlabSchema{}, ctx.Err()
	}
}

// waiters is how many callers wait on host's probe in flight.
func (p *gitlabSchemaProbes) waiters(host string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if flight := p.flights[host]; flight != nil {
		return flight.waiters
	}
	return 0
}

// probeSchema sends the PRManageSchema probe and caches its answer.
func (f *gitlabForge) probeSchema(ctx context.Context, client *forgeapi.Client, host string) (gitlabSchema, error) {
	var raw struct {
		Rereview *struct {
			Name string `json:"name"`
		} `json:"rereview"`
		UpdateInput  *gitlabInputType `json:"updateInput"`
		SetReviewers *gitlabInputType `json:"setReviewers"`
		SetAssignees *gitlabInputType `json:"setAssignees"`
		SetLabels    *gitlabInputType `json:"setLabels"`
	}
	if _, err := client.GraphQL(ctx, "PRManageSchema", gitlabManageSchemaQuery, nil, &raw); err != nil {
		return gitlabSchema{}, err
	}
	schema := gitlabSchema{
		rereview:     raw.Rereview != nil,
		override:     raw.UpdateInput.has("overrideRequestedChanges"),
		setReviewers: raw.SetReviewers.has("operationMode"),
		setAssignees: raw.SetAssignees.has("operationMode"),
		setLabels:    raw.SetLabels.has("operationMode"),
	}
	f.schemas.put(host, schema, f.core.nowFn())
	return schema, nil
}

type gitlabGraphQLUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

func (u gitlabGraphQLUser) person() PRPerson {
	return PRPerson{Login: u.Username, Name: u.Name, ID: strings.TrimPrefix(u.ID, "gid://gitlab/User/")}
}

type gitlabLabelNode struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Color string `json:"color"`
}

func (l gitlabLabelNode) label() PRLabel {
	return PRLabel{ID: l.ID, Name: l.Title, Color: normalizeColor(l.Color)}
}

type gitlabManageRaw struct {
	Project *struct {
		MergeRequest *struct {
			UserPermissions struct {
				UpdateMergeRequest bool `json:"updateMergeRequest"`
				AdminMergeRequest  bool `json:"adminMergeRequest"`
			} `json:"userPermissions"`
			Reviewers struct {
				Nodes []struct {
					gitlabGraphQLUser
					Interaction *struct {
						ReviewState string `json:"reviewState"`
					} `json:"mergeRequestInteraction"`
				} `json:"nodes"`
			} `json:"reviewers"`
			Assignees struct {
				Nodes []gitlabGraphQLUser `json:"nodes"`
			} `json:"assignees"`
			Labels struct {
				Nodes []gitlabLabelNode `json:"nodes"`
			} `json:"labels"`
			MergeabilityChecks []struct {
				Identifier string `json:"identifier"`
				Status     string `json:"status"`
			} `json:"mergeabilityChecks"`
		} `json:"mergeRequest"`
	} `json:"project"`
}

// gitlabManage is the Details read and what it adds to readiness.
type gitlabManage struct {
	manage PRManage
	// bypassed is the merge request's requested-changes check passing
	// while a reviewer still requests changes.
	bypassed bool
}

// readManage is the Details read of mr (see the file comment).
func (f *gitlabForge) readManage(ctx context.Context, client *forgeapi.Client, ref PRReference, mr gitlabMRRaw) (gitlabManage, error) {
	schema, err := f.manageSchema(ctx, client, ref.Host)
	if err != nil {
		return gitlabManage{}, err
	}
	op, doc := "PRManage", gitlabManageQuery
	if schema.override {
		op, doc = "PRManageChecks", gitlabManageChecksQuery
	}
	var raw gitlabManageRaw
	vars := map[string]any{"fullPath": ref.Project(), "iid": strconv.Itoa(ref.Number)}
	if _, err := client.GraphQL(ctx, op, doc, vars, &raw); err != nil {
		return gitlabManage{}, err
	}
	if raw.Project == nil || raw.Project.MergeRequest == nil {
		return gitlabManage{}, fmt.Errorf("GitLab answered no merge request %s!%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	node := raw.Project.MergeRequest
	manage := emptyManage()
	changes := false
	for _, reviewer := range node.Reviewers.Nodes {
		state := ReviewerPending
		if reviewer.Interaction != nil {
			switch reviewer.Interaction.ReviewState {
			case "APPROVED":
				state = ReviewerApproved
			case "REQUESTED_CHANGES":
				state, changes = ReviewerChangesRequested, true
			case "REVIEWED":
				state = ReviewerCommented
			}
		}
		manage.Reviewers = append(manage.Reviewers, PRReviewer{PRPerson: reviewer.person(), State: state})
	}
	for _, assignee := range node.Assignees.Nodes {
		manage.Assignees = append(manage.Assignees, assignee.person())
	}
	for _, label := range node.Labels.Nodes {
		manage.Labels = append(manage.Labels, label.label())
	}
	state := NormalizePRState(mr.State)
	open, update, admin := state == "open", node.UserPermissions.UpdateMergeRequest, node.UserPermissions.AdminMergeRequest
	// Where the host has the mergeability checks, the requested-changes
	// check says whether they block; elsewhere the detailed status.
	blocked := strings.EqualFold(mr.DetailedMergeStatus, "requested_changes")
	out := gitlabManage{}
	for _, check := range node.MergeabilityChecks {
		if check.Identifier != "REQUESTED_CHANGES" {
			continue
		}
		blocked = check.Status == "FAILED"
		out.bypassed = check.Status == "SUCCESS" && changes && open
	}
	manage.Can = PRManageCan{
		EditTitle:       update,
		EditDescription: update,
		Reviewers:       open && admin && schema.setReviewers,
		Assignees:       admin && schema.setAssignees,
		Labels:          admin && schema.setLabels,
		Close:           open && update,
		Reopen:          state == "closed" && update,
		ConvertToDraft:  open && update && !mr.Draft && !mr.WorkInProgress,
		Rerequest:       open && update && schema.rereview,
		Bypass:          open && blocked && schema.override && mr.User.CanMerge,
	}
	out.manage = manage
	return out, nil
}

// unavailableManage is the Details read that failed with err: nothing
// shown, nothing offered, and the sentence saying so.
func unavailableManage(ref PRReference, err error) gitlabManage {
	manage := emptyManage()
	manage.Unavailable = manageUnavailableSentence(ref, err)
	return gitlabManage{manage: manage}
}

// apply puts the Details read on the detail: the changes-requested
// blocker says whether the viewer may bypass it, and requested changes
// GitLab reports overridden read as a settled row.
func (m gitlabManage) apply(detail *PRDetail) {
	detail.Manage = m.manage
	for i := range detail.Readiness.Blockers {
		if detail.Readiness.Blockers[i].Kind == BlockerChangesRequested {
			detail.Readiness.Blockers[i].Bypassable = m.manage.Can.Bypass
		}
	}
	if m.bypassed {
		detail.Readiness.Settled = append(detail.Readiness.Settled, MergeBlocker{Kind: BlockerChangesRequested, Text: "Requested changes bypassed", Bypassed: true})
	}
}

// gitlabStateEventsMaxPages bounds a state events read's requests: past
// it, page 1 and the newest pages.
const gitlabStateEventsMaxPages = 3

// gitlabStateEventRaw is one resource state event: GitLab records a
// close, a reopen and a merge as these rather than as system notes.
type gitlabStateEventRaw struct {
	ID        int64           `json:"id"`
	User      gitlabAuthorRaw `json:"user"`
	CreatedAt string          `json:"created_at"`
	State     string          `json:"state"`
}

// readGitLabStateEvents reads the merge request's state events, oldest
// first as GitLab lists them, in at most gitlabStateEventsMaxPages
// requests: every page when they fit; past them page 1 and the last
// pages (by X-Total-Pages; without it, the first pages).
func readGitLabStateEvents(ctx context.Context, client *forgeapi.Client, ref PRReference) ([]gitlabStateEventRaw, error) {
	read := func(page int) ([]gitlabStateEventRaw, *forgeapi.Response, error) {
		query := url.Values{"per_page": {strconv.Itoa(gitlabPageSize)}}
		if page > 1 {
			query.Set("page", strconv.Itoa(page))
		}
		resp, err := client.Do(ctx, forgeapi.Request{Path: gitlabMRPath(ref) + "/resource_state_events", Query: query})
		if err != nil {
			return nil, nil, err
		}
		var events []gitlabStateEventRaw
		if err := json.Unmarshal(resp.Body, &events); err != nil {
			return nil, nil, fmt.Errorf("GitLab state events: decode response: %w", err)
		}
		return events, resp, nil
	}
	out, resp, err := read(1)
	if err != nil {
		return nil, err
	}
	page := 1
	if total, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("X-Total-Pages"))); err == nil && total > gitlabStateEventsMaxPages {
		for p := total - gitlabStateEventsMaxPages + 2; p <= total; p++ {
			events, _, err := read(p)
			if err != nil {
				return nil, err
			}
			out = append(out, events...)
		}
		return out, nil
	}
	for n := 1; n < gitlabStateEventsMaxPages && advanceGitLabPage(resp, &page); n++ {
		var events []gitlabStateEventRaw
		if events, resp, err = read(page); err != nil {
			return nil, err
		}
		out = append(out, events...)
	}
	return out, nil
}

// gitlabStateEventRetries holds the merge requests whose state events
// read is failing: each is asked again once due, its wait the Core's
// prLookupErrorDelay of its consecutive failures. A success forgets it;
// at most gitlabCacheMax are held, the soonest due dropped first.
type gitlabStateEventRetries struct {
	mu      sync.Mutex
	entries map[string]gitlabStateEventsRetry
}

type gitlabStateEventsRetry struct {
	failures int
	due      time.Time
}

// due reports whether key may be read at now, and its failures so far.
func (r *gitlabStateEventRetries) due(key string, now time.Time) (bool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	retry, ok := r.entries[key]
	return !ok || !now.Before(retry.due), retry.failures
}

func (r *gitlabStateEventRetries) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = nil
}

func (r *gitlabStateEventRetries) succeeded(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
}

func (r *gitlabStateEventRetries) failed(key string, retry gitlabStateEventsRetry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]gitlabStateEventsRetry)
	}
	if _, ok := r.entries[key]; !ok && len(r.entries) >= gitlabCacheMax {
		soonest := ""
		for k, entry := range r.entries {
			if soonest == "" || entry.due.Before(r.entries[soonest].due) {
				soonest = k
			}
		}
		delete(r.entries, soonest)
	}
	r.entries[key] = retry
}

// stateEvents is the events read's state events. A failure leaves the
// events without them (the merge request's merged_at and closed_at still
// stand in for a merge and a close), is logged, and is not asked again
// until its retry is due, so a failing endpoint costs one request and one
// log line per wait, not per tick.
func (f *gitlabForge) stateEvents(ctx context.Context, client *forgeapi.Client, ref PRReference) []gitlabStateEventRaw {
	key, now := ref.Key(), f.core.nowFn()
	due, failures := f.stateEventRetries.due(key, now)
	if !due {
		return nil
	}
	states, err := readGitLabStateEvents(ctx, client, ref)
	if err == nil {
		if failures > 0 {
			f.stateEventRetries.succeeded(key)
		}
		return states
	}
	if ctx.Err() != nil {
		return nil
	}
	next := now.Add(f.core.prLookupErrorDelay(failures + 1))
	f.stateEventRetries.failed(key, gitlabStateEventsRetry{failures: failures + 1, due: next})
	log.Printf("gitlab: state events of %s!%d on %s unavailable, closes and reopens left out until %s: %v", ref.Project(), ref.Number, ref.Host, next.Format(time.RFC3339), err)
	return nil
}

// gitlabStateEvents maps state events to forge events. GitLab records a
// reopen as "opened" (the state events API's own value; the first open
// is not recorded), "reopened" on some servers.
func gitlabStateEvents(states []gitlabStateEventRaw) []ReviewEvent {
	var out []ReviewEvent
	for _, state := range states {
		kind := map[string]string{"closed": EventClosed, "opened": EventReopened, "reopened": EventReopened, "merged": EventMerged}[state.State]
		if kind == "" {
			continue
		}
		out = append(out, ReviewEvent{ID: "state:" + strconv.FormatInt(state.ID, 10), Kind: kind, ActorLogin: state.User.Username, ActorName: state.User.Name, At: state.CreatedAt})
	}
	return out
}
