package git

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/forgeapi"
)

// The GitLab half of the Merge section's reads: readiness and merge
// options from the merge request, its approvals and the project's merge
// settings, and the forge events from the system notes the discussions
// read already carries. See docs/specs/pull-requests.md.

// gitlabCacheTTL is how long one login's merge settings of a project, and
// one login's viewer on a host, answer every detail read. A read after it
// goes to the transport, which revalidates it with the ETag GitLab sends
// (a 304 while unchanged). Both caches are keyed on the credential's
// fingerprint (forgeapi.Client.CredentialFingerprint), so another login
// never reads the previous one's viewer or permissions.
const gitlabCacheTTL = 10 * time.Minute

// gitlabCacheMax bounds each cache's entries; the oldest read goes first.
const gitlabCacheMax = 256

// gitlabProjectSettings is the subset of projects/P the merge options read.
type gitlabProjectSettings struct {
	// ID and ForkedFrom are the project's id and, for a fork, the project
	// it was forked from, where a fork checkout's merge requests open
	// (ListOpenPRs).
	ID         int64 `json:"id"`
	ForkedFrom *struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"forked_from_project"`
	MergeMethod                               string `json:"merge_method"`
	SquashOption                              string `json:"squash_option"`
	RemoveSourceBranchAfterMerge              bool   `json:"remove_source_branch_after_merge"`
	OnlyAllowMergeIfPipelineSucceeds          bool   `json:"only_allow_merge_if_pipeline_succeeds"`
	OnlyAllowMergeIfAllDiscussionsAreResolved bool   `json:"only_allow_merge_if_all_discussions_are_resolved"`
	// MergeTrainsEnabled is a Premium or Ultimate project's merge trains
	// ("Indicates if merge trains are enabled."; exposed to every reader
	// of a licensed project by ee/lib/ee/api/entities/project.rb at
	// v18.0.0-ee, absent on Free).
	MergeTrainsEnabled bool `json:"merge_trains_enabled"`
}

// gitlabDeveloperAccess is the access level GitLab's default branch
// protection lets merge.
const gitlabDeveloperAccess = 30

// gitlabProjectAccessQuery reads the viewer's role in a project.
// maxAccessLevel is "Maximum access level of the current user in the
// project." (GraphQL reference at 18.0), so it counts a group the project
// is shared with; REST permissions do not (their group_access is the
// project's own group and its ancestors).
var gitlabProjectAccessQuery = forgeapi.NewDocument(forgeapi.ForgeGitLab, `query ProjectAccess($fullPath: ID!) {
  project(fullPath: $fullPath) { maxAccessLevel { integerValue } }
}`)

// gitlabCache holds values read from GitLab for gitlabCacheTTL, at most
// gitlabCacheMax of them.
type gitlabCache[V any] struct {
	mu      sync.Mutex
	entries map[string]gitlabCacheEntry[V]
}

type gitlabCacheEntry[V any] struct {
	value  V
	readAt time.Time
}

func (c *gitlabCache[V]) get(key string, now time.Time) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || now.Sub(entry.readAt) >= gitlabCacheTTL || now.Before(entry.readAt) {
		var zero V
		return zero, false
	}
	return entry.value, true
}

// clear forgets every entry.
func (c *gitlabCache[V]) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

func (c *gitlabCache[V]) put(key string, value V, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]gitlabCacheEntry[V])
	}
	if _, ok := c.entries[key]; !ok && len(c.entries) >= gitlabCacheMax {
		oldest, oldestAt := "", time.Time{}
		for k, entry := range c.entries {
			if oldest == "" || entry.readAt.Before(oldestAt) {
				oldest, oldestAt = k, entry.readAt
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[key] = gitlabCacheEntry[V]{value: value, readAt: now}
}

// projectSettings is repo's merge settings as the current login
// reads them, read from projects/P at most once per login per
// gitlabCacheTTL. The entry is published under the credential that got
// the answer (Response.Credential), which a 401 inside the read can have
// replaced since the lookup, so no login reads another's permissions.
func (f *gitlabForge) projectSettings(ctx context.Context, client *forgeapi.Client, ref RepoReference) (gitlabProjectSettings, error) {
	fp, err := client.CredentialFingerprint(ctx)
	if err != nil {
		return gitlabProjectSettings{}, err
	}
	key := func(fp string) string { return fp + " " + ref.Host + " " + ref.Project() }
	if settings, ok := f.projects.get(key(fp), f.core.nowFn()); ok {
		return settings, nil
	}
	var settings gitlabProjectSettings
	resp, err := client.JSON(ctx, forgeapi.Request{Path: gitlabProjectPath(ref.Project())}, &settings)
	if err != nil {
		return gitlabProjectSettings{}, err
	}
	f.projects.put(key(resp.Credential), settings, f.core.nowFn())
	return settings, nil
}

// gitlabTrainCar is a car of the merge trains API.
type gitlabTrainCar struct {
	ID           int64  `json:"id"`
	TargetBranch string `json:"target_branch"`
}

// gitlabTrainPageSize is the cars one position read asks for: a car past
// it reads as queued without a position.
const gitlabTrainPageSize = 100

// readGitLabMergeTrain is an open merge request's merge train on a merge
// train project (Premium, Ultimate). Only an armed auto-merge can be on
// the train, so one that is not costs no request. An armed one reads its
// car (GET merge_trains/merge_requests/N, "Get merge train information
// for the requested merge request", 404 when it has none: it joins once
// its checks pass), and a car its position: GitLab orders a train's
// active cars by id (MergeTrains::Train#all_cars at v18.0.0-ee), so it
// is the car's place among the target branch's cars of GET
// merge_trains?scope=active&sort=asc, read without the branch in the
// path, which the route does not escape.
func readGitLabMergeTrain(ctx context.Context, client *forgeapi.Client, ref PRReference, mr gitlabMRRaw, canRemove bool) (*MergeQueue, error) {
	queue := &MergeQueue{Required: true}
	if !mr.MergeWhenPipelineSucceeds {
		return queue, nil
	}
	queue.CanRemove = canRemove
	trains := gitlabProjectPath(ref.Project()) + "/merge_trains"
	var car gitlabTrainCar
	if _, err := client.JSON(ctx, forgeapi.Request{Path: trains + "/merge_requests/" + strconv.Itoa(ref.Number)}, &car); err != nil {
		if status, ok := errors.AsType[*forgeapi.StatusError](err); ok && status.Status == http.StatusNotFound {
			return queue, nil
		}
		return nil, err
	}
	queue.Queued = true
	var cars []gitlabTrainCar
	query := url.Values{"scope": {"active"}, "sort": {"asc"}, "per_page": {strconv.Itoa(gitlabTrainPageSize)}}
	resp, err := client.JSON(ctx, forgeapi.Request{Path: trains, Query: query}, &cars)
	if err != nil {
		return nil, err
	}
	size := 0
	for _, each := range cars {
		if each.TargetBranch != mr.TargetBranch {
			continue
		}
		size++
		if each.ID == car.ID {
			queue.Position = size
		}
	}
	if strings.TrimSpace(resp.Header.Get("X-Next-Page")) == "" {
		queue.Size = size
	}
	return queue, nil
}

// viewerAccess is the current login's access level in repo (10 guest to
// 50 owner, 0 for none), read with gitlabProjectAccessQuery at most once
// per login per gitlabCacheTTL and published under the credential that
// answered, as projectSettings is.
func (f *gitlabForge) viewerAccess(ctx context.Context, client *forgeapi.Client, ref RepoReference) (int, error) {
	fp, err := client.CredentialFingerprint(ctx)
	if err != nil {
		return 0, err
	}
	key := func(fp string) string { return fp + " " + ref.Host + " " + ref.Project() }
	if access, ok := f.access.get(key(fp), f.core.nowFn()); ok {
		return access, nil
	}
	var raw struct {
		Project *struct {
			MaxAccessLevel struct {
				IntegerValue int `json:"integerValue"`
			} `json:"maxAccessLevel"`
		} `json:"project"`
	}
	resp, err := client.GraphQL(ctx, gitlabProjectAccessQuery, map[string]any{"fullPath": ref.Project()}, &raw)
	if err != nil {
		return 0, err
	}
	if raw.Project == nil {
		return 0, fmt.Errorf("GitLab answered no project %s on %s", ref.Project(), ref.Host)
	}
	f.access.put(key(resp.Credential), raw.Project.MaxAccessLevel.IntegerValue, f.core.nowFn())
	return raw.Project.MaxAccessLevel.IntegerValue, nil
}

// viewerLogin is the username host's current login reads as (GET user),
// read at most once per login per gitlabCacheTTL and published under the
// credential that answered (Response.Credential), as projectSettings is.
// It never fails the read it serves: a refusal or an unreadable answer is
// logged and cached as unknown ("") for the TTL, so a login that cannot
// read its user costs no request per tick; a cancelled, rate-limited or
// unreachable read, or a credential that cannot be read, caches nothing
// and the next read asks again. A read whose answer depends on knowing
// the login (the project list's attention states) uses readViewerLogin
// and fails instead.
func (f *gitlabForge) viewerLogin(ctx context.Context, client *forgeapi.Client, host string) string {
	viewer, _ := f.readViewerLogin(ctx, client, host)
	return viewer
}

// readViewerLogin is viewerLogin with the failure: the username, or the
// error that left it unknown. A refusal is still cached as unknown for
// the TTL and returned as that cached error each time.
func (f *gitlabForge) readViewerLogin(ctx context.Context, client *forgeapi.Client, host string) (string, error) {
	viewer, err := f.readViewer(ctx, client, host)
	return viewer.Username, err
}

// gitlabViewer is the account host's current login reads as: its
// username and numeric id (the events feed is addressed by the id).
type gitlabViewer struct {
	Username string `json:"username"`
	ID       int64  `json:"id"`
}

// readViewer is readViewerLogin with the account's id.
func (f *gitlabForge) readViewer(ctx context.Context, client *forgeapi.Client, host string) (gitlabViewer, error) {
	fp, err := client.CredentialFingerprint(ctx)
	if err != nil {
		return gitlabViewer{}, err
	}
	key := func(fp string) string { return fp + " " + host }
	if viewer, ok := f.viewers.get(key(fp), f.core.nowFn()); ok {
		if viewer.Username == "" {
			return gitlabViewer{}, fmt.Errorf("gitlab: the login on %s could not be read; retried in %s", host, gitlabCacheTTL)
		}
		return viewer, nil
	}
	var user gitlabViewer
	resp, err := client.JSON(ctx, forgeapi.Request{Path: "user"}, &user)
	if err != nil {
		_, limited := errors.AsType[*forgeapi.RateLimitedError](err)
		_, transient := errors.AsType[*forgeapi.TransientError](err)
		if ctx.Err() != nil || limited || transient {
			return gitlabViewer{}, err
		}
		log.Printf("gitlab: reading the login on %s failed; merge requests read as not the viewer's for %s: %v", host, gitlabCacheTTL, err)
		// The refusal is the lookup's login's only while the client still
		// holds that credential: a 401 inside the call can have replaced it,
		// and the new login was never refused, so nothing is cached then.
		if after, err := client.CredentialFingerprint(ctx); err == nil && after == fp {
			f.viewers.put(key(fp), gitlabViewer{}, f.core.nowFn())
		}
		return gitlabViewer{}, err
	}
	f.viewers.put(key(resp.Credential), user, f.core.nowFn())
	return user, nil
}

// gitlabFillMerge fills the detail's readiness, merge options, end of
// life and whether viewer (the login's username, empty when unknown)
// authored it from the merge request, its approvals, the project's
// settings, the viewer's access level (viewerAccess, needed only when it
// cannot merge) and the read's events.
func gitlabFillMerge(detail *PRDetail, mr gitlabMRRaw, approvals gitlabApprovalsRaw, settings gitlabProjectSettings, access int, events []ReviewEvent, viewer string) {
	detail.ViewerIsAuthor = viewer != "" && strings.EqualFold(viewer, mr.Author.Username)
	detail.Readiness = gitlabReadiness(mr, approvals, settings)
	detail.Readiness.Approvals = ApprovalCount{Given: len(approvals.ApprovedBy), Required: approvals.ApprovalsRequired}
	detail.Merge = gitlabMergeOptions(mr, approvals, settings, access, detail.Readiness, events, detail.ViewerIsAuthor)
	switch NormalizePRState(mr.State) {
	case "merged":
		detail.MergedAt = mr.MergedAt
		detail.MergedBy = mr.MergedBy.Username
		if detail.MergedBy == "" {
			detail.MergedBy = mr.MergeUser.Username
		}
	case "closed":
		detail.ClosedAt = mr.ClosedAt
	}
}

// gitlabReadiness maps detailed_merge_status, which names the first check
// that fails, to the readiness state and its blocker, and adds the
// blockers the merge request's own fields show beside it (approvals left,
// conflicts, unresolved threads or a failed pipeline the project
// requires). A merge request GitLab is still checking stays checking.
func gitlabReadiness(mr gitlabMRRaw, approvals gitlabApprovalsRaw, settings gitlabProjectSettings) MergeReadiness {
	switch NormalizePRState(mr.State) {
	case "merged":
		return MergeReadiness{State: MergeStateMerged}
	case "closed":
		return MergeReadiness{State: MergeStateClosed}
	case "locked":
		// GitLab locks a merge request while it merges it.
		return MergeReadiness{State: MergeStateChecking}
	}
	status := strings.ToLower(strings.TrimSpace(mr.DetailedMergeStatus))
	draft := mr.Draft || mr.WorkInProgress || status == "draft_status"
	out := MergeReadiness{State: MergeStateBlocked}
	switch status {
	case "mergeable":
		out.State = MergeStateReady
	case "checking", "unchecked", "preparing", "approvals_syncing":
		out.State = MergeStateChecking
	default:
		out.Blockers = append(out.Blockers, gitlabStatusBlocker(status, mr, approvals))
	}
	if out.State == MergeStateBlocked || draft {
		out.Blockers = gitlabFieldBlockers(out.Blockers, mr, approvals, settings)
	}
	if draft {
		out.State = MergeStateDraft
		if !slices.ContainsFunc(out.Blockers, func(b MergeBlocker) bool { return b.Kind == BlockerDraft }) {
			out.Blockers = append([]MergeBlocker{gitlabDraftBlocker()}, out.Blockers...)
		}
	}
	out.Settled = gitlabSettled(out.Blockers, mr, approvals, settings)
	return out
}

func gitlabDraftBlocker() MergeBlocker {
	return MergeBlocker{Kind: BlockerDraft, Text: "This merge request is a draft"}
}

// gitlabStatusBlocker is the blocker one detailed_merge_status value
// names. A value with no entry is BlockerOther carrying the value.
func gitlabStatusBlocker(status string, mr gitlabMRRaw, approvals gitlabApprovalsRaw) MergeBlocker {
	pipelineURL := ""
	if mr.HeadPipeline != nil {
		pipelineURL = mr.HeadPipeline.WebURL
	}
	rule := func(text string) MergeBlocker { return MergeBlocker{Kind: BlockerRules, Text: text} }
	switch {
	case status == "not_approved":
		return gitlabApprovalsBlocker(approvals)
	case status == "ci_must_pass":
		return MergeBlocker{Kind: BlockerChecks, Text: "The pipeline must succeed", Detail: gitlabPipelineDetail(mr.HeadPipeline), URL: pipelineURL}
	case status == "ci_still_running":
		return MergeBlocker{Kind: BlockerChecks, Text: "The pipeline is still running", Detail: gitlabPipelineDetail(mr.HeadPipeline), URL: pipelineURL}
	case status == "status_checks_must_pass":
		return MergeBlocker{Kind: BlockerChecks, Text: "External status checks must pass"}
	case status == "discussions_not_resolved":
		return MergeBlocker{Kind: BlockerThreads, Text: "All threads must be resolved"}
	case status == "conflict":
		return MergeBlocker{Kind: BlockerConflicts, Text: "Merge conflicts must be resolved"}
	case status == "need_rebase":
		return MergeBlocker{Kind: BlockerRebase, Text: fmt.Sprintf("The branch must be rebased onto %s", mr.TargetBranch)}
	case status == "draft_status":
		return gitlabDraftBlocker()
	case status == "requested_changes":
		return MergeBlocker{Kind: BlockerChangesRequested, Text: "A reviewer requested changes"}
	case status == "merge_request_blocked":
		return rule("Blocked by a merge request it depends on")
	case status == "jira_association_missing":
		return rule("The title or description must reference a Jira issue")
	case strings.HasPrefix(status, "security_policy_"):
		return MergeBlocker{Kind: BlockerRules, Text: "A security policy blocks this merge", Detail: status}
	case status == "locked_paths":
		return rule("A path this merge request changes is locked")
	case status == "locked_lfs_files":
		return rule("An LFS file this merge request changes is locked")
	case status == "title_regex":
		return rule("The title does not match the project's required pattern")
	case status == "merge_time":
		if mr.MergeAfter != "" {
			return MergeBlocker{Kind: BlockerRules, Text: "Merging is scheduled for later", Detail: "Not before " + mr.MergeAfter}
		}
		return rule("Merging is scheduled for later")
	case status == "commits_status":
		// The source branch is missing or empty: no fix on the row.
		return MergeBlocker{Kind: BlockerOther, Text: "The source branch is missing or has no commits"}
	}
	return MergeBlocker{Kind: BlockerOther, Text: status}
}

// gitlabFieldBlockers adds to blockers what the merge request's fields
// show that detailed_merge_status, naming one check, may not: approvals
// left, conflicts, and the unresolved threads or failed pipeline the
// project requires. A kind already listed is not repeated.
func gitlabFieldBlockers(blockers []MergeBlocker, mr gitlabMRRaw, approvals gitlabApprovalsRaw, settings gitlabProjectSettings) []MergeBlocker {
	has := func(kind string) bool {
		return slices.ContainsFunc(blockers, func(b MergeBlocker) bool { return b.Kind == kind })
	}
	if approvals.ApprovalsRequired > 0 && approvals.ApprovalsLeft > 0 && !has(BlockerApprovals) {
		blockers = append(blockers, gitlabApprovalsBlocker(approvals))
	}
	if mr.HasConflicts && !has(BlockerConflicts) {
		blockers = append(blockers, MergeBlocker{Kind: BlockerConflicts, Text: "Merge conflicts must be resolved"})
	}
	if settings.OnlyAllowMergeIfAllDiscussionsAreResolved && !mr.BlockingDiscussionsResolved && !has(BlockerThreads) {
		blockers = append(blockers, MergeBlocker{Kind: BlockerThreads, Text: "All threads must be resolved"})
	}
	if settings.OnlyAllowMergeIfPipelineSucceeds && mr.HeadPipeline != nil && gitlabPipelineFailed(mr.HeadPipeline.Status) && !has(BlockerChecks) {
		blockers = append(blockers, MergeBlocker{Kind: BlockerChecks, Text: "The pipeline must succeed", Detail: gitlabPipelineDetail(mr.HeadPipeline), URL: mr.HeadPipeline.WebURL})
	}
	return blockers
}

func gitlabPipelineFailed(status string) bool {
	switch status {
	case "failed", "canceled":
		return true
	}
	return false
}

func gitlabApprovalsBlocker(approvals gitlabApprovalsRaw) MergeBlocker {
	left := max(approvals.ApprovalsLeft, 1)
	text := "Needs 1 more approval"
	if left > 1 {
		text = fmt.Sprintf("Needs %d more approvals", left)
	}
	detail := ""
	if approvals.ApprovalsRequired > 0 {
		detail = fmt.Sprintf("%d required", approvals.ApprovalsRequired)
	}
	if names := gitlabApproverNames(approvals); names != "" {
		detail = strings.TrimSuffix(names+" approved · "+detail, " · ")
	}
	return MergeBlocker{Kind: BlockerApprovals, Text: text, Detail: detail}
}

func gitlabApproverNames(approvals gitlabApprovalsRaw) string {
	names := make([]string, 0, len(approvals.ApprovedBy))
	for _, approval := range approvals.ApprovedBy {
		name := approval.User.Name
		if name == "" {
			name = approval.User.Username
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

func gitlabPipelineDetail(pipeline *gitlabPipelineRaw) string {
	if pipeline == nil || pipeline.Status == "" {
		return ""
	}
	return "Pipeline " + strings.ReplaceAll(pipeline.Status, "_", " ")
}

// gitlabSettled lists the conditions that pass, for every open state:
// approvals met (on GitLab Free, where nothing is required, the count),
// a passed pipeline, no conflicts, and resolved threads when the project
// requires them. A kind listed as a blocker is not settled.
func gitlabSettled(blockers []MergeBlocker, mr gitlabMRRaw, approvals gitlabApprovalsRaw, settings gitlabProjectSettings) []MergeBlocker {
	blocked := func(kind string) bool {
		return slices.ContainsFunc(blockers, func(b MergeBlocker) bool { return b.Kind == kind })
	}
	var out []MergeBlocker
	names := gitlabApproverNames(approvals)
	switch {
	case blocked(BlockerApprovals):
	case approvals.ApprovalsRequired == 0:
		out = append(out, MergeBlocker{Kind: BlockerApprovals, Text: gitlabApprovalCount(len(approvals.ApprovedBy)), Detail: names})
	case approvals.ApprovalsLeft <= 0:
		out = append(out, MergeBlocker{Kind: BlockerApprovals, Text: fmt.Sprintf("Approved %d of %d", approvals.ApprovalsRequired, approvals.ApprovalsRequired), Detail: names})
	}
	if !blocked(BlockerChecks) && mr.HeadPipeline != nil && mr.HeadPipeline.Status == "success" {
		out = append(out, MergeBlocker{Kind: BlockerChecks, Text: "Pipeline passed", URL: mr.HeadPipeline.WebURL})
	}
	if !blocked(BlockerConflicts) && !mr.HasConflicts {
		out = append(out, MergeBlocker{Kind: BlockerConflicts, Text: "No conflicts"})
	}
	if !blocked(BlockerThreads) && settings.OnlyAllowMergeIfAllDiscussionsAreResolved && mr.BlockingDiscussionsResolved {
		out = append(out, MergeBlocker{Kind: BlockerThreads, Text: "All threads resolved"})
	}
	return out
}

func gitlabApprovalCount(n int) string {
	switch n {
	case 0:
		return "No approvals"
	case 1:
		return "1 approval"
	}
	return fmt.Sprintf("%d approvals", n)
}

// gitlabMethods is what the project allows, its default first, or nil when
// the project answer carried no merge_method. merge_method merge is a merge
// commit, rebase_merge a merge commit on a rebased branch (offered as
// merge and rebase, which GitLab runs the same way), ff a fast-forward
// (rebase). The squash option adds squash, first when the merge request's
// own squash flag is set; always leaves squash alone.
func gitlabMethods(settings gitlabProjectSettings, squash bool) []string {
	var base []string
	switch settings.MergeMethod {
	case "merge":
		base = []string{MergeMethodMerge}
	case "rebase_merge":
		base = []string{MergeMethodMerge, MergeMethodRebase}
	case "ff":
		base = []string{MergeMethodRebase}
	default:
		return nil
	}
	switch settings.SquashOption {
	case "never":
		return base
	case "always":
		return []string{MergeMethodSquash}
	}
	if squash {
		return append([]string{MergeMethodSquash}, base...)
	}
	return append(base, MergeMethodSquash)
}

// gitlabAutoMergePrefix opens the system note of an armed auto-merge.
const gitlabAutoMergePrefix = "enabled an automatic merge"

// gitlabMergeOptions derives the menu from the merge request's fields.
// author is whether the viewer opened the merge request: GitLab lets the
// author cancel an auto-merge without merge rights and refuses the
// author's approval unless the project allows it (user_can_approve).
// access is the viewer's access level, 0 when unknown.
func gitlabMergeOptions(mr gitlabMRRaw, approvals gitlabApprovalsRaw, settings gitlabProjectSettings, access int, readiness MergeReadiness, events []ReviewEvent, author bool) MergeOptions {
	open := NormalizePRState(mr.State) == "open"
	fork := mr.SourceProjectID != 0 && mr.ProjectID != 0 && mr.SourceProjectID != mr.ProjectID
	out := MergeOptions{
		Methods:          gitlabMethods(settings, mr.Squash),
		ViewerCanMerge:   mr.User.CanMerge,
		ViewerCanApprove: approvals.UserCanApprove,
		ViewerApproved:   approvals.UserHasApproved,
		// GitLab answers no rebase or draft permission of its own: a rebase
		// is offered with merge rights, the draft toggle to them and the
		// author, and an approver may always withdraw their approval.
		ViewerCanUpdateBranch: open && mr.User.CanMerge,
		ViewerCanUnapprove:    open && approvals.UserHasApproved,
		ViewerCanMarkReady:    open && (mr.User.CanMerge || author),
	}
	if !open {
		return out
	}
	// A merge train takes no branch delete (gitlabJoinMergeTrain).
	if !fork && !settings.MergeTrainsEnabled {
		out.DeleteBranchOffered = true
		out.DeleteBranchDefault = settings.RemoveSourceBranchAfterMerge
		if mr.ForceRemoveSourceBranch != nil {
			out.DeleteBranchDefault = *mr.ForceRemoveSourceBranch
		}
	}
	// The server decides what an auto-merge waits for; offer it whenever it
	// could apply.
	out.AutoMergeAvailable = mr.HeadPipeline != nil || readiness.State == MergeStateChecking || mr.User.CanMerge
	if mr.MergeWhenPipelineSucceeds {
		out.AutoMerge = gitlabAutoMerge(mr, out.Methods, events, author)
	}
	if !mr.User.CanMerge {
		if access > 0 && access < gitlabDeveloperAccess {
			out.CannotMergeReason = "Your role in this project cannot merge."
		} else {
			out.CannotMergeReason = fmt.Sprintf("You do not have merge access to %s.", mr.TargetBranch)
		}
		out.CannotUpdateBranchReason = "Rebasing this branch needs merge access to this project."
		if !author {
			out.CannotMarkReadyReason = "Only the author and people with merge access can mark this merge request ready."
		}
	}
	switch {
	case !approvals.UserCanApprove && !approvals.UserHasApproved:
		out.CannotApproveReason = "You are not an eligible approver."
		if author {
			out.CannotApproveReason = "You cannot approve your own merge request."
		}
	case approvals.UserCanApprove && approvals.RequirePasswordToApprove:
		// gitlabApproveRequest refuses the approve itself.
		out.ViewerCanApprove = false
		out.CannotApproveReason = gitlabApprovalPasswordReason
	}
	return out
}

// gitlabAutoMerge is the armed auto-merge: who set it (merge_user), with
// the method the merge will use, and when and for what from its latest
// system note when the read has the notes. A viewer who can merge, or
// the author, can cancel it.
func gitlabAutoMerge(mr gitlabMRRaw, methods []string, events []ReviewEvent, author bool) *AutoMergeRequest {
	method := MergeMethodMerge
	if mr.Squash {
		method = MergeMethodSquash
	} else if i := slices.IndexFunc(methods, func(m string) bool { return m != MergeMethodSquash }); i >= 0 {
		method = methods[i]
	}
	out := &AutoMergeRequest{
		Method:    method,
		ByLogin:   mr.MergeUser.Username,
		ByName:    mr.MergeUser.Name,
		WaitsFor:  "all merge checks pass",
		CanCancel: mr.User.CanMerge || author,
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != EventAutoMergeSet {
			continue
		}
		out.At = events[i].At
		if waits, ok := strings.CutPrefix(events[i].Text, gitlabAutoMergePrefix+" when "); ok && waits != "" {
			out.WaitsFor = waits
		}
		break
	}
	return out
}

// gitlabEvents maps the system notes and the resource state events (the
// closes, reopens and merges GitLab records as these, not as notes) to
// forge events in time order. Without state events (a read that did not
// ask for events) it adds the merge and the close the merge request
// itself reports.
func gitlabEvents(mr gitlabMRRaw, notes []gitlabNoteRaw, states []gitlabStateEventRaw) []ReviewEvent {
	out := gitlabStateEvents(states)
	added := len(out) > 0
	for i := range out {
		if out[i].Kind == EventMerged {
			out[i].Ref = mr.MergeCommitSHA
		}
	}
	for _, note := range notes {
		kind, ref, text, ok := gitlabNoteEvent(note.Body)
		if !ok {
			continue
		}
		out = append(out, ReviewEvent{
			ID:         strconv.FormatInt(note.ID, 10),
			Kind:       kind,
			ActorLogin: note.Author.Username,
			ActorName:  note.Author.Name,
			At:         note.CreatedAt,
			Text:       text,
			Ref:        ref,
		})
	}
	has := func(kind string) bool {
		return slices.ContainsFunc(out, func(e ReviewEvent) bool { return e.Kind == kind })
	}
	switch NormalizePRState(mr.State) {
	case "merged":
		if mr.MergedAt != "" && !has(EventMerged) {
			by := mr.MergedBy
			if by.Username == "" {
				by = mr.MergeUser
			}
			out = append(out, ReviewEvent{ID: "merged", Kind: EventMerged, ActorLogin: by.Username, ActorName: by.Name, At: mr.MergedAt, Ref: mr.MergeCommitSHA})
			added = true
		}
	case "closed":
		if mr.ClosedAt != "" && !has(EventClosed) {
			out = append(out, ReviewEvent{ID: "closed:" + mr.ClosedAt, Kind: EventClosed, ActorLogin: mr.ClosedBy.Username, ActorName: mr.ClosedBy.Name, At: mr.ClosedAt})
			added = true
		}
	}
	if added {
		// GitLab's timestamps share one RFC 3339 UTC spelling, so they
		// order as strings.
		slices.SortStableFunc(out, func(a, b ReviewEvent) int { return strings.Compare(a.At, b.At) })
	}
	return out
}

// gitlabNoteEvent maps one system note body to its event kind, the ref it
// names and the forge's own wording where it adds to the kind. The
// phrasings are GitLab's (app/services/system_notes/), recorded in
// docs/references/forge-api-measurements.md. Pushes ("added N commits")
// are not events: GitLab writes no force-push or rebase wording, and the
// feed builds pushes from git.
func gitlabNoteEvent(body string) (kind, ref, text string, ok bool) {
	line := strings.TrimSpace(strings.SplitN(body, "\n", 2)[0])
	plain := strings.ReplaceAll(line, "**", "")
	switch {
	case plain == "approved this merge request":
		return EventApproved, "", "", true
	case plain == "unapproved this merge request":
		return EventUnapproved, "", "", true
	case plain == "merged":
		return EventMerged, "", "", true
	case plain == "closed" || strings.HasPrefix(plain, "closed via "):
		return EventClosed, "", "", true
	case plain == "reopened":
		return EventReopened, "", "", true
	case strings.HasPrefix(plain, "marked this merge request as draft"), strings.HasPrefix(plain, "marked as a Work In Progress"):
		return EventMarkedDraft, "", "", true
	case strings.HasPrefix(plain, "marked this merge request as ready"), strings.HasPrefix(plain, "unmarked as a Work In Progress"):
		return EventMarkedReady, "", "", true
	case strings.HasPrefix(plain, gitlabAutoMergePrefix):
		sha := ""
		if _, rest, found := strings.Cut(plain, " for "); found {
			sha, _, _ = strings.Cut(rest, " ")
		}
		return EventAutoMergeSet, sha, plain, true
	case plain == "canceled the automatic merge":
		return EventAutoMergeCancel, "", "", true
	case strings.HasPrefix(plain, "aborted the automatic merge"):
		return EventAutoMergeCancel, "", plain, true
	// A merge train's (SystemNotes::MergeTrainService at v18.0.0-ee),
	// whose link is dropped.
	case strings.HasPrefix(plain, "started a [merge train]"), strings.HasPrefix(plain, "added this merge request to the [merge train]"),
		plain == "enabled automatic add to merge train when checks pass":
		return EventAutoMergeSet, "", gitlabDropLinks(plain), true
	case strings.HasPrefix(plain, "removed this merge request from the merge train"), plain == "cancelled automatic add to merge train",
		strings.HasPrefix(plain, "aborted automatic add to merge train"):
		return EventAutoMergeCancel, "", plain, true
	case strings.HasPrefix(plain, "changed target branch from "),
		strings.HasPrefix(plain, "deleted the `") && strings.Contains(plain, "This merge request now targets the `"):
		return EventBaseChanged, gitlabLastCode(plain), "", true
	case strings.HasPrefix(plain, "requested review from "):
		return EventReviewRequested, gitlabReviewers(strings.TrimPrefix(plain, "requested review from ")), "", true
	}
	return "", "", "", false
}

// gitlabMarkdownLink is a Markdown link, its text the first group.
var gitlabMarkdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

// gitlabDropLinks is text with each Markdown link replaced by its text.
func gitlabDropLinks(text string) string {
	return gitlabMarkdownLink.ReplaceAllString(text, "$1")
}

// gitlabLastCode is the last `quoted` name in a note.
func gitlabLastCode(text string) string {
	end := strings.LastIndex(text, "`")
	if end <= 0 {
		return ""
	}
	start := strings.LastIndex(text[:end], "`")
	if start < 0 {
		return ""
	}
	return text[start+1 : end]
}

// gitlabReviewers reads "@a, @b, and @c" (and the "@a and @b" spelling),
// up to a trailing " and removed ...", as "a, b, c".
func gitlabReviewers(list string) string {
	if i := strings.Index(list, " and removed "); i >= 0 {
		list = list[:i]
	}
	list = strings.ReplaceAll(list, ", and ", ", ")
	list = strings.ReplaceAll(list, " and ", ", ")
	var names []string
	for _, name := range strings.Split(list, ", ") {
		if name = strings.TrimPrefix(strings.TrimSpace(name), "@"); name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}
