package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// The GitHub PR read: one GraphQL operation, PRTick, per pump tick, with
// dedicated page operations for any connection longer than one page. The
// request document is the same every tick; the parts a caller does not
// want are left out with @include so their cost is not paid. Selection
// text is shared by Go constants rather than GraphQL fragments, so each
// operation is self-contained.

// githubReadPageSize is the page size of every connection the read
// selects, the GraphQL API's maximum.
const githubReadPageSize = 100

// githubReadMaxPages bounds the pages read of one connection in one read:
// review threads, conversation comments, one thread's comments and the
// rollup's check contexts each stop after this many pages
// (githubReadMaxPages * githubReadPageSize nodes). A PR past it shows the
// first nodes in the forge's order, and a threads read past it says so
// (PRRead.ThreadsTruncated).
const githubReadMaxPages = prReadMaxPages

const (
	githubPageInfoFields = `pageInfo { hasNextPage endCursor }`
	githubActor          = `author { login ... on User { name } }`

	// githubViewerCommentFields is what edit, delete and the "edited" mark
	// read from a comment of either kind, with its reaction groups while
	// the read's $wantReactions is set: every document that selects it
	// declares and sends that variable.
	githubViewerCommentFields = `lastEditedAt url viewerCanUpdate viewerCanDelete viewerCanReact reactionGroups @include(if: $wantReactions) ` + githubReactionGroupFields
	// githubReactionFields is a comment's reaction groups: GitHub answers
	// every content, so the count is the reactors' totalCount (no nodes:
	// the names would cost a connection per comment).
	githubReactionFields      = `reactionGroups ` + githubReactionGroupFields
	githubReactionGroupFields = `{ content viewerHasReacted reactors { totalCount } }`
	githubThreadCommentFields = `id fullDatabaseId ` + githubActor + ` body createdAt ` + githubViewerCommentFields + ` replyTo { id fullDatabaseId }`
	githubThreadFields        = `id isResolved isOutdated path line startLine originalLine originalStartLine diffSide startDiffSide subjectType ` +
		`viewerCanResolve viewerCanUnresolve viewerCanReply ` +
		`comments(first: $pageSize) { ` + githubPageInfoFields + ` nodes { ` + githubThreadCommentFields + ` } }`
	githubCommentFields = `id fullDatabaseId ` + githubActor + ` body createdAt ` + githubViewerCommentFields + ` isMinimized`
	// isRequired is the forge's own answer to whether the PR's base
	// requires the context, branch protection and rulesets alike, so it
	// needs the PR's $number in every document that selects contexts. A
	// check run and its workflow run are optional parts of the read
	// ($wantCheckRuns, $wantWorkflowRuns: githubReadParts).
	githubContextFields = `__typename ` +
		`... on CheckRun @include(if: $wantCheckRuns) { databaseId name status conclusion startedAt completedAt detailsUrl isRequired(pullRequestNumber: $number) checkSuite @include(if: $wantWorkflowRuns) { workflowRun { databaseId url workflow { name } } } } ` +
		`... on StatusContext { context state targetUrl createdAt description isRequired(pullRequestNumber: $number) }`
	githubActorAs = `login ... on User { name }`
	// githubDetailFields is the PR half of the detail. The merge fields
	// feed Readiness and Merge (github_readiness.go): refUpdateRule is the
	// base's branch protection as a non-admin may read it (null on a
	// ruleset-only repository), rules the active rulesets' rules on the
	// base, latestOpinionatedReviews each reviewer's latest approval or
	// change request (latestReviews shows a later comment instead), and
	// threadStates the review threads' resolution alone.
	githubDetailFields = `title body state isDraft baseRefName baseRefOid headRefName headRefOid url ` +
		`additions deletions changedFiles mergeable mergeStateStatus reviewDecision ` + githubActor + ` ` +
		`latestReviews(first: 100) { nodes { ` + githubActor + ` body submittedAt state commit { oid } } } ` +
		`merged mergedAt mergedBy { ` + githubActorAs + ` } closedAt viewerDidAuthor isCrossRepository viewerCanUpdateBranch maintainerCanModify ` +
		`isMergeQueueEnabled isInMergeQueue mergeQueueEntry { position enqueuedAt enqueuer { ` + githubActorAs + ` } } mergeQueue { entries { totalCount } } ` +
		`viewerCanEnableAutoMerge viewerCanDisableAutoMerge ` +
		`autoMergeRequest { enabledAt mergeMethod enabledBy { ` + githubActorAs + ` } } ` +
		`latestOpinionatedReviews(first: 100) { nodes { id state viewerDidAuthor authorCanPushToRepository commit { oid } ` + githubActor + ` } } ` +
		`threadStates: reviewThreads(first: 100) { ` + githubPageInfoFields + ` nodes { isResolved } } ` +
		`baseRef { refUpdateRule { requiredApprovingReviewCount requiredStatusCheckContexts requiresCodeOwnerReviews requiresConversationResolution } ` +
		`rules(first: 100) { nodes { type parameters { __typename ` +
		`... on PullRequestParameters { requiredApprovingReviewCount requireCodeOwnerReview requiredReviewThreadResolution allowedMergeMethods } ` +
		`... on RequiredStatusChecksParameters { requiredStatusChecks { context } } } } } } ` +
		githubManageFields
	// githubRepositoryMergeFields is the repository half of the detail:
	// the merge methods and settings the Merge menu offers.
	// viewerPermission is selected beside it on every tick, since the CI
	// actions read it too.
	githubRepositoryMergeFields = `mergeCommitAllowed squashMergeAllowed rebaseMergeAllowed deleteBranchOnMerge autoMergeAllowed viewerDefaultMergeMethod owner { __typename }`
	// githubTimelineItemTypes are the timeline items the Conversation feed
	// shows as forge events (github_readiness.go maps them).
	githubTimelineItemTypes = `PULL_REQUEST_REVIEW, REVIEW_DISMISSED_EVENT, MERGED_EVENT, CLOSED_EVENT, REOPENED_EVENT, ` +
		`CONVERT_TO_DRAFT_EVENT, READY_FOR_REVIEW_EVENT, AUTO_MERGE_ENABLED_EVENT, AUTO_SQUASH_ENABLED_EVENT, ` +
		`AUTO_REBASE_ENABLED_EVENT, AUTO_MERGE_DISABLED_EVENT, HEAD_REF_FORCE_PUSHED_EVENT, BASE_REF_CHANGED_EVENT, ` +
		`REVIEW_REQUESTED_EVENT`
	githubTimelineActor  = `actor { ` + githubActorAs + ` }`
	githubTimelineFields = `__typename ` +
		`... on PullRequestReview { id state submittedAt ` + githubActor + ` commit { oid } } ` +
		`... on ReviewDismissedEvent { id createdAt ` + githubTimelineActor + ` dismissalMessage review { author { login } } } ` +
		`... on MergedEvent { id createdAt ` + githubTimelineActor + ` commit { oid } } ` +
		`... on ClosedEvent { id createdAt ` + githubTimelineActor + ` } ` +
		`... on ReopenedEvent { id createdAt ` + githubTimelineActor + ` } ` +
		`... on ConvertToDraftEvent { id createdAt ` + githubTimelineActor + ` } ` +
		`... on ReadyForReviewEvent { id createdAt ` + githubTimelineActor + ` } ` +
		`... on AutoMergeEnabledEvent { id createdAt ` + githubTimelineActor + ` } ` +
		`... on AutoSquashEnabledEvent { id createdAt ` + githubTimelineActor + ` } ` +
		`... on AutoRebaseEnabledEvent { id createdAt ` + githubTimelineActor + ` } ` +
		`... on AutoMergeDisabledEvent { id createdAt ` + githubTimelineActor + ` reason } ` +
		`... on HeadRefForcePushedEvent { id createdAt ` + githubTimelineActor + ` afterCommit { oid } } ` +
		`... on BaseRefChangedEvent { id createdAt ` + githubTimelineActor + ` currentRefName } ` +
		`... on ReviewRequestedEvent { id createdAt ` + githubTimelineActor + ` requestedReviewer { __typename ... on User { login } ... on Bot { login } ... on Mannequin { login } ... on Team @include(if: $wantTeams) { combinedSlug } } }`
	githubTimelinePageInfoFields = `pageInfo { hasPreviousPage startCursor }`
)

// githubPRTickQuery reads a PR's detail, review threads and conversation
// comments, timeline events, and the head commit's check rollup.
// The repository's viewerPermission is always selected (the merge options
// and the CI actions read it); $wantDetail gates the detail, the viewer
// and the repository's merge settings, $wantThreads the threads and comments, $wantEvents the newest
// page of timeline items, and $wantChecks the rollup, which both the
// detail's check summary and readiness and the CI pipeline are built from.
// $pageSize is the threads' and comments' page (githubReadLevels), and
// $wantManage, $wantTeams, $wantCheckRuns and $wantWorkflowRuns the
// optional parts (githubReadParts).
var githubPRTickQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PRTick($owner: String!, $name: String!, $number: Int!, $wantDetail: Boolean!, $wantThreads: Boolean!, $wantEvents: Boolean!, $wantChecks: Boolean!, $wantReactions: Boolean!, $pageSize: Int!, $wantManage: Boolean!, $wantTeams: Boolean!, $wantCheckRuns: Boolean!, $wantWorkflowRuns: Boolean!) {
  viewer @include(if: $wantDetail) { login }
  repository(owner: $owner, name: $name) {
    viewerPermission
    ... @include(if: $wantDetail) { `+githubRepositoryMergeFields+` }
    pullRequest(number: $number) {
      number headRefName
      ... @include(if: $wantDetail) { `+githubDetailFields+` }
      commits(last: 1) @include(if: $wantChecks) { nodes { commit { id oid statusCheckRollup { contexts(first: 100) { `+githubPageInfoFields+` nodes { `+githubContextFields+` } } } } } }
      ... @include(if: $wantThreads) { state viewerCanApplySuggestion }
      reviewThreads(first: $pageSize) @include(if: $wantThreads) { `+githubPageInfoFields+` nodes { `+githubThreadFields+` } }
      comments(first: $pageSize) @include(if: $wantThreads) { `+githubPageInfoFields+` nodes { `+githubCommentFields+` } }
      timelineItems(last: 100, itemTypes: [`+githubTimelineItemTypes+`]) @include(if: $wantEvents) { `+githubTimelinePageInfoFields+` nodes { `+githubTimelineFields+` } }
    }
  }
}`)

var githubPRThreadsPageQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PRThreadsPage($owner: String!, $name: String!, $number: Int!, $after: String!, $wantReactions: Boolean!, $pageSize: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: $pageSize, after: $after) { `+githubPageInfoFields+` nodes { `+githubThreadFields+` } }
    }
  }
}`)

var githubPRCommentsPageQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PRCommentsPage($owner: String!, $name: String!, $number: Int!, $after: String!, $wantReactions: Boolean!, $pageSize: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      comments(first: $pageSize, after: $after) { `+githubPageInfoFields+` nodes { `+githubCommentFields+` } }
    }
  }
}`)

var githubThreadCommentsPageQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query ThreadCommentsPage($threadID: ID!, $after: String!, $wantReactions: Boolean!) {
  node(id: $threadID) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $after) { `+githubPageInfoFields+` nodes { `+githubThreadCommentFields+` } }
    }
  }
}`)

var githubThreadStatesPageQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PRThreadStatesPage($owner: String!, $name: String!, $number: Int!, $after: String!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) { `+githubPageInfoFields+` nodes { isResolved } }
    }
  }
}`)

// githubPRTimelinePageQuery reads the timeline page before $before: the
// tick reads the newest items, so older pages are read backwards.
var githubPRTimelinePageQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PRTimelinePage($owner: String!, $name: String!, $number: Int!, $before: String!, $wantTeams: Boolean!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      timelineItems(last: 100, before: $before, itemTypes: [`+githubTimelineItemTypes+`]) { `+githubTimelinePageInfoFields+` nodes { `+githubTimelineFields+` } }
    }
  }
}`)

var githubRollupContextsPageQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query RollupContextsPage($commitID: ID!, $number: Int!, $after: String!, $wantCheckRuns: Boolean!, $wantWorkflowRuns: Boolean!) {
  node(id: $commitID) {
    ... on Commit {
      statusCheckRollup { contexts(first: 100, after: $after) { `+githubPageInfoFields+` nodes { `+githubContextFields+` } } }
    }
  }
}`)

type githubPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// githubBigInt is a GraphQL BigInt (a comment's fullDatabaseId, its REST
// id), which GitHub encodes as a string; null is zero.
type githubBigInt int64

func (b *githubBigInt) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*b = 0
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("GitHub BigInt %s is not a string: %w", data, err)
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fmt.Errorf("GitHub BigInt %q: %w", text, err)
	}
	*b = githubBigInt(n)
	return nil
}

// githubActorRaw is a GraphQL Actor selected as
// `author { login ... on User { name } }`. A Bot or Mannequin has no name
// key and a User without a display name answers null or ""; all decode
// to "". A deleted account is a null author, which decodes to "".
type githubActorRaw struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

// githubViewerCommentRaw is githubViewerCommentFields; lastEditedAt is
// null for a comment never edited.
type githubViewerCommentRaw struct {
	LastEditedAt    string                   `json:"lastEditedAt"`
	URL             string                   `json:"url"`
	ViewerCanUpdate bool                     `json:"viewerCanUpdate"`
	ViewerCanDelete bool                     `json:"viewerCanDelete"`
	ViewerCanReact  bool                     `json:"viewerCanReact"`
	ReactionGroups  []githubReactionGroupRaw `json:"reactionGroups"`
}

// githubReactionGroupRaw is one reactionGroups entry.
type githubReactionGroupRaw struct {
	Content          string `json:"content"`
	ViewerHasReacted bool   `json:"viewerHasReacted"`
	Reactors         struct {
		TotalCount int `json:"totalCount"`
	} `json:"reactors"`
}

type githubThreadCommentRaw struct {
	ID         string         `json:"id"`
	DatabaseID githubBigInt   `json:"fullDatabaseId"`
	Body       string         `json:"body"`
	CreatedAt  string         `json:"createdAt"`
	Author     githubActorRaw `json:"author"`
	githubViewerCommentRaw
	ReplyTo *struct {
		ID         string       `json:"id"`
		DatabaseID githubBigInt `json:"fullDatabaseId"`
	} `json:"replyTo"`
}

type githubThreadCommentsRaw struct {
	PageInfo githubPageInfo           `json:"pageInfo"`
	Nodes    []githubThreadCommentRaw `json:"nodes"`
}

type githubThreadRaw struct {
	ID                 string                  `json:"id"`
	IsResolved         bool                    `json:"isResolved"`
	IsOutdated         bool                    `json:"isOutdated"`
	Path               string                  `json:"path"`
	Line               *int                    `json:"line"`
	StartLine          *int                    `json:"startLine"`
	OriginalLine       *int                    `json:"originalLine"`
	OriginalStartLine  *int                    `json:"originalStartLine"`
	DiffSide           string                  `json:"diffSide"`
	StartDiffSide      string                  `json:"startDiffSide"`
	SubjectType        string                  `json:"subjectType"`
	ViewerCanResolve   bool                    `json:"viewerCanResolve"`
	ViewerCanUnresolve bool                    `json:"viewerCanUnresolve"`
	ViewerCanReply     bool                    `json:"viewerCanReply"`
	Comments           githubThreadCommentsRaw `json:"comments"`
}

type githubThreadsRaw struct {
	PageInfo githubPageInfo    `json:"pageInfo"`
	Nodes    []githubThreadRaw `json:"nodes"`
}

type githubCommentRaw struct {
	ID          string         `json:"id"`
	DatabaseID  githubBigInt   `json:"fullDatabaseId"`
	Body        string         `json:"body"`
	CreatedAt   string         `json:"createdAt"`
	IsMinimized bool           `json:"isMinimized"`
	Author      githubActorRaw `json:"author"`
	githubViewerCommentRaw
}

type githubCommentsRaw struct {
	PageInfo githubPageInfo     `json:"pageInfo"`
	Nodes    []githubCommentRaw `json:"nodes"`
}

// githubContextRaw is one StatusCheckRollupContext: a CheckRun or a
// StatusContext, told apart by __typename. Times are null when absent.
type githubContextRaw struct {
	Typename    string `json:"__typename"`
	DatabaseID  int64  `json:"databaseId"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt"`
	DetailsURL  string `json:"detailsUrl"`
	CheckSuite  *struct {
		WorkflowRun *struct {
			DatabaseID int64  `json:"databaseId"`
			URL        string `json:"url"`
			Workflow   *struct {
				Name string `json:"name"`
			} `json:"workflow"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
	Context    string `json:"context"`
	State      string `json:"state"`
	TargetURL  string `json:"targetUrl"`
	CreatedAt  string `json:"createdAt"`
	IsRequired bool   `json:"isRequired"`
}

type githubContextsRaw struct {
	PageInfo githubPageInfo     `json:"pageInfo"`
	Nodes    []githubContextRaw `json:"nodes"`
}

type githubRollupRaw struct {
	Contexts githubContextsRaw `json:"contexts"`
}

// githubReviewRaw is one latestReviews node.
type githubReviewRaw struct {
	Body        string         `json:"body"`
	SubmittedAt string         `json:"submittedAt"`
	State       string         `json:"state"`
	Author      githubActorRaw `json:"author"`
	Commit      struct {
		OID string `json:"oid"`
	} `json:"commit"`
}

type githubPullRaw struct {
	Number           int            `json:"number"`
	Title            string         `json:"title"`
	Body             string         `json:"body"`
	State            string         `json:"state"`
	IsDraft          bool           `json:"isDraft"`
	BaseRefName      string         `json:"baseRefName"`
	BaseRefOID       string         `json:"baseRefOid"`
	HeadRefName      string         `json:"headRefName"`
	HeadRefOID       string         `json:"headRefOid"`
	URL              string         `json:"url"`
	Additions        int            `json:"additions"`
	Deletions        int            `json:"deletions"`
	ChangedFiles     int            `json:"changedFiles"`
	Mergeable        string         `json:"mergeable"`
	MergeStateStatus string         `json:"mergeStateStatus"`
	ReviewDecision   string         `json:"reviewDecision"`
	Author           githubActorRaw `json:"author"`
	// ViewerCanApplySuggestion is selected with the threads: whether the
	// viewer may commit a suggestion to the head branch.
	ViewerCanApplySuggestion bool `json:"viewerCanApplySuggestion"`
	LatestReviews            struct {
		Nodes []githubReviewRaw `json:"nodes"`
	} `json:"latestReviews"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				ID                string           `json:"id"`
				OID               string           `json:"oid"`
				StatusCheckRollup *githubRollupRaw `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	ReviewThreads githubThreadsRaw  `json:"reviewThreads"`
	Comments      githubCommentsRaw `json:"comments"`
	TimelineItems githubTimelineRaw `json:"timelineItems"`
	githubPullMergeRaw
	githubPullManageRaw
}

// githubTickRaw is PRTick's data.
type githubTickRaw struct {
	Viewer *struct {
		Login string `json:"login"`
	} `json:"viewer"`
	Repository *struct {
		githubRepositoryMergeRaw
		PullRequest *githubPullRaw `json:"pullRequest"`
	} `json:"repository"`
}

// pull returns the tick's pull request, or an error when the answer has
// none (GitHub reports a missing one as a GraphQL error first).
func (t githubTickRaw) pull(ref PRReference) (*githubPullRaw, error) {
	if t.Repository == nil || t.Repository.PullRequest == nil {
		return nil, fmt.Errorf("GitHub answered no pull request %s#%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	return t.Repository.PullRequest, nil
}

// githubPullVars are the variables naming a pull request.
func githubPullVars(ref PRReference) (map[string]any, error) {
	owner, repo, err := splitGitHubProject(ref.Project())
	if err != nil {
		return nil, err
	}
	return map[string]any{"owner": owner, "name": repo, "number": ref.Number}, nil
}

// githubReactionsTooLarge is ReactionsUnavailable for a read that fits
// only without the comments' reaction groups.
const githubReactionsTooLarge = "Reactions are not shown: this pull request has too many comments to read them."

// ReadPR answers the parts want names from one PRTick request, plus a
// page request for each connection past its first page and, for CI, the
// REST jobs lists the steps of stepsFor jobs need, in the shape the
// repository's and the PR's earlier reads left (github_read_shape.go). A
// read refused for an optional part runs again without it, and the
// detail says what is not shown; a threads read too large to answer runs
// again one level smaller (without reactions, saying so as
// githubReactionsTooLarge, then in smaller pages). Each step is
// remembered for githubReadShapeTTL, so later ticks start from it.
func (f *githubForge) ReadPR(ctx context.Context, ref PRReference, want PRReadParts, prev *CIPipeline, stepsFor []string) (PRRead, error) {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return PRRead{}, err
	}
	fingerprint, err := client.CredentialFingerprint(ctx)
	if err != nil {
		return PRRead{}, err
	}
	partsKey := ref.Repository().Key() + "\x00" + fingerprint
	levelKey := ref.Key()
	now := f.core.nowFn()
	shape := f.readMemory.shape(partsKey, levelKey, want.Threads, now)
	remembered := maps.Clone(shape.excluded)
	for {
		out, err := f.readPR(ctx, client, ref, want, prev, stepsFor, shape)
		if err == nil {
			if want.Threads && shape.level > 0 {
				f.readMemory.levels.put(levelKey, shape.level, f.core.nowFn().Add(githubReadShapeTTL))
			}
			return out, nil
		}
		if parts, remember, ok := githubPartsOf(err, want, shape); ok {
			shape.excluded = maps.Clone(shape.excluded)
			for _, part := range parts {
				sentence := githubPartSentence(part, ref, err)
				shape.excluded[part] = sentence
				if remember {
					remembered[part] = sentence
				}
			}
			if remember {
				f.readMemory.parts.put(partsKey, maps.Clone(remembered), f.core.nowFn().Add(githubReadShapeTTL))
			}
			continue
		}
		if want.Threads && shape.level+1 < len(githubReadLevels) && githubTooLarge(err) {
			shape.level++
			continue
		}
		return PRRead{}, err
	}
}

func (f *githubForge) readPR(ctx context.Context, client *forgeapi.Client, ref PRReference, want PRReadParts, prev *CIPipeline, stepsFor []string, shape githubReadShape) (PRRead, error) {
	vars, err := githubPullVars(ref)
	if err != nil {
		return PRRead{}, err
	}
	vars["wantDetail"] = want.Detail
	vars["wantThreads"] = want.Threads
	vars["wantEvents"] = want.Events
	vars["wantChecks"] = want.Detail || want.CI
	vars["wantReactions"] = want.Threads && shape.reactions()
	vars["pageSize"] = shape.pageSize()
	shape.setVars(vars, githubReadParts...)
	var raw githubTickRaw
	if _, err := client.GraphQL(ctx, githubPRTickQuery, vars, &raw); err != nil {
		return PRRead{}, err
	}
	pull, err := raw.pull(ref)
	if err != nil {
		return PRRead{}, err
	}
	var checks []CheckStatus
	var contexts []githubContextRaw
	if want.Detail || want.CI {
		if contexts, err = f.readRollupContexts(ctx, client, ref, pull, shape); err != nil {
			return PRRead{}, err
		}
		checks = githubCheckStatuses(contexts)
	}
	var out PRRead
	if want.Detail {
		viewer := ""
		if raw.Viewer != nil {
			viewer = raw.Viewer.Login
		}
		out.Detail = githubPRDetail(pull, viewer, checks)
		merge := githubMergeInputs{pull: pull, repo: raw.Repository.githubRepositoryMergeRaw, contexts: contexts}
		if merge.needsThreadStates() {
			if err := f.readThreadStates(ctx, client, ref, pull); err != nil {
				return PRRead{}, err
			}
		}
		merge.checksUnknown = !shape.has(githubPartCheckRuns)
		merge.manageUnknown = !shape.has(githubPartManage)
		out.Detail.Readiness = merge.readiness()
		out.Detail.Merge = merge.options(out.Detail.Readiness)
		out.Detail.Manage = merge.manage()
		if sentence, left := shape.excluded[githubPartManage]; left {
			out.Detail.Manage = emptyManage()
			out.Detail.Manage.Unavailable = sentence
		}
		if !shape.has(githubPartTeams) {
			out.Detail.Manage.Can.ReviewerTeams = false
		}
		out.Detail.Unavailable = shape.unavailable()
		if pull.Merged || strings.EqualFold(pull.State, "MERGED") {
			out.Detail.MergedAt = pull.MergedAt
			if pull.MergedBy != nil {
				out.Detail.MergedBy = pull.MergedBy.Login
			}
		}
		if !strings.EqualFold(pull.State, "OPEN") {
			out.Detail.ClosedAt = pull.ClosedAt
		}
	}
	if want.Events {
		if out.Events, err = f.readEvents(ctx, client, ref, pull, shape); err != nil {
			return PRRead{}, err
		}
	}
	if want.Threads {
		var truncated bool
		if out.Threads, truncated, err = f.readThreads(ctx, client, ref, pull, shape); err != nil {
			return PRRead{}, err
		}
		if !shape.reactions() {
			out.ReactionsUnavailable = githubReactionsTooLarge
		}
		if truncated {
			out.ThreadsTruncated = threadsTruncatedSentence(ref)
		}
	}
	if want.CI {
		if out.CI, err = f.githubPipeline(ctx, client, ref.Repository(), checks, githubCIPermission(raw.Repository.ViewerPermission), prev, stepsFor); err != nil {
			return PRRead{}, err
		}
		if notes := shape.ciUnavailable(); notes != "" {
			out.CI.ActionsUnavailable = strings.TrimSpace(notes + " " + out.CI.ActionsUnavailable)
		}
		if len(out.CI.Stages) > 0 {
			out.CI.Ref = pull.HeadRefName
			if len(pull.Commits.Nodes) > 0 {
				out.CI.SHA = pull.Commits.Nodes[0].Commit.OID
			}
		}
		out.HasCI = true
	}
	return out, nil
}

// readRollupContexts returns the head commit's check contexts, paging
// past the first page. A PR whose head has no checks answers none.
func (f *githubForge) readRollupContexts(ctx context.Context, client *forgeapi.Client, ref PRReference, pull *githubPullRaw, shape githubReadShape) ([]githubContextRaw, error) {
	if len(pull.Commits.Nodes) == 0 || pull.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
		return nil, nil
	}
	commit := pull.Commits.Nodes[0].Commit
	contexts := commit.StatusCheckRollup.Contexts
	nodes := contexts.Nodes
	page := contexts.PageInfo
	for pages := 1; page.HasNextPage && pages < githubReadMaxPages; pages++ {
		if commit.ID == "" || page.EndCursor == "" {
			return nil, errors.New("GitHub check rollup has another page but no commit id or cursor to read it by")
		}
		var raw struct {
			Node *struct {
				StatusCheckRollup *githubRollupRaw `json:"statusCheckRollup"`
			} `json:"node"`
		}
		vars := map[string]any{"commitID": commit.ID, "number": ref.Number, "after": page.EndCursor}
		shape.setVars(vars, githubPartCheckRuns, githubPartWorkflowRuns)
		if _, err := client.GraphQL(ctx, githubRollupContextsPageQuery, vars, &raw); err != nil {
			return nil, err
		}
		if raw.Node == nil || raw.Node.StatusCheckRollup == nil {
			return nil, fmt.Errorf("GitHub answered no check rollup for commit %s", commit.ID)
		}
		nodes = append(nodes, raw.Node.StatusCheckRollup.Contexts.Nodes...)
		page = raw.Node.StatusCheckRollup.Contexts.PageInfo
	}
	return nodes, nil
}

// readThreads returns the review threads, each with all its comments, then
// the conversation comments as path-less single-comment threads, paging
// every connection past its first page up to githubReadMaxPages;
// truncated says a connection had more. GitHub keeps PR conversation
// comments (issue comments) flat, outside review threads; the review pane
// lists them with the threads. Each connection reads at most
// githubReadMaxPages pages of githubReadPageSize nodes, whatever the
// shape's page size; truncated says one had more.
func (f *githubForge) readThreads(ctx context.Context, client *forgeapi.Client, ref PRReference, pull *githubPullRaw, shape githubReadShape) (_ []ReviewThread, truncated bool, _ error) {
	vars, err := githubPullVars(ref)
	if err != nil {
		return nil, false, err
	}
	reactions := shape.reactions()
	vars["wantReactions"] = reactions
	vars["pageSize"] = shape.pageSize()
	maxPages := githubReadMaxPages * githubReadPageSize / shape.pageSize()
	threads := pull.ReviewThreads.Nodes
	page := pull.ReviewThreads.PageInfo
	for pages := 1; page.HasNextPage && pages < maxPages; pages++ {
		var raw githubTickRaw
		if _, err := client.GraphQL(ctx, githubPRThreadsPageQuery, withAfter(vars, page.EndCursor), &raw); err != nil {
			return nil, false, err
		}
		next, err := raw.pull(ref)
		if err != nil {
			return nil, false, err
		}
		threads = append(threads, next.ReviewThreads.Nodes...)
		page = next.ReviewThreads.PageInfo
	}
	truncated = page.HasNextPage
	for i := range threads {
		if err := f.readThreadComments(ctx, client, &threads[i], reactions); err != nil {
			return nil, false, err
		}
		truncated = truncated || threads[i].Comments.PageInfo.HasNextPage
	}

	comments := pull.Comments.Nodes
	page = pull.Comments.PageInfo
	for pages := 1; page.HasNextPage && pages < maxPages; pages++ {
		var raw githubTickRaw
		if _, err := client.GraphQL(ctx, githubPRCommentsPageQuery, withAfter(vars, page.EndCursor), &raw); err != nil {
			return nil, false, err
		}
		next, err := raw.pull(ref)
		if err != nil {
			return nil, false, err
		}
		comments = append(comments, next.Comments.Nodes...)
		page = next.Comments.PageInfo
	}
	out := githubReviewThreads(threads)
	githubMarkSuggestions(out, pull)
	truncated = truncated || page.HasNextPage
	return append(out, githubConversationThreads(comments)...), truncated, nil
}

// readThreadComments completes one thread's comments past their first
// page, with their reaction groups when reactions is set, up to
// githubReadMaxPages pages and githubReadMaxPages*githubReadPageSize
// comments (the first page is the shape's size, the rest 100).
func (f *githubForge) readThreadComments(ctx context.Context, client *forgeapi.Client, thread *githubThreadRaw, reactions bool) error {
	page := thread.Comments.PageInfo
	for pages := 1; page.HasNextPage && pages < githubReadMaxPages && len(thread.Comments.Nodes) < githubReadMaxPages*githubReadPageSize; pages++ {
		if page.EndCursor == "" {
			return fmt.Errorf("GitHub review thread %s has another page of comments but no cursor", thread.ID)
		}
		var raw struct {
			Node *struct {
				Comments *githubThreadCommentsRaw `json:"comments"`
			} `json:"node"`
		}
		if _, err := client.GraphQL(ctx, githubThreadCommentsPageQuery, map[string]any{"threadID": thread.ID, "after": page.EndCursor, "wantReactions": reactions}, &raw); err != nil {
			return err
		}
		if raw.Node == nil || raw.Node.Comments == nil {
			return fmt.Errorf("GitHub answered no review thread %s", thread.ID)
		}
		thread.Comments.Nodes = append(thread.Comments.Nodes, raw.Node.Comments.Nodes...)
		page = raw.Node.Comments.PageInfo
	}
	thread.Comments.PageInfo = page
	return nil
}

// readThreadStates completes the detail's thread resolution states past
// their first page.
func (f *githubForge) readThreadStates(ctx context.Context, client *forgeapi.Client, ref PRReference, pull *githubPullRaw) error {
	vars, err := githubPullVars(ref)
	if err != nil {
		return err
	}
	page := pull.ThreadStates.PageInfo
	for pages := 1; page.HasNextPage && pages < githubReadMaxPages; pages++ {
		if page.EndCursor == "" {
			return errors.New("GitHub review thread states have another page but no cursor")
		}
		var raw struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads githubThreadStatesRaw `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if _, err := client.GraphQL(ctx, githubThreadStatesPageQuery, withAfter(vars, page.EndCursor), &raw); err != nil {
			return err
		}
		if raw.Repository == nil || raw.Repository.PullRequest == nil {
			return fmt.Errorf("GitHub answered no pull request %s#%d on %s", ref.Project(), ref.Number, ref.Host)
		}
		pull.ThreadStates.Nodes = append(pull.ThreadStates.Nodes, raw.Repository.PullRequest.ReviewThreads.Nodes...)
		page = raw.Repository.PullRequest.ReviewThreads.PageInfo
	}
	pull.ThreadStates.PageInfo = page
	return nil
}

// readEvents returns the PR's forge events oldest first: the tick's
// newest timeline page, then older pages read backwards, at most
// githubReadMaxPages pages (the newest items when the timeline is longer).
func (f *githubForge) readEvents(ctx context.Context, client *forgeapi.Client, ref PRReference, pull *githubPullRaw, shape githubReadShape) ([]ReviewEvent, error) {
	vars, err := githubPullVars(ref)
	if err != nil {
		return nil, err
	}
	shape.setVars(vars, githubPartTeams)
	pages := [][]githubTimelineItemRaw{pull.TimelineItems.Nodes}
	page := pull.TimelineItems.PageInfo
	for n := 1; page.HasPreviousPage && n < githubReadMaxPages; n++ {
		if page.StartCursor == "" {
			return nil, errors.New("GitHub timeline has an earlier page but no cursor to read it by")
		}
		before := maps.Clone(vars)
		before["before"] = page.StartCursor
		var raw githubTickRaw
		if _, err := client.GraphQL(ctx, githubPRTimelinePageQuery, before, &raw); err != nil {
			return nil, err
		}
		next, err := raw.pull(ref)
		if err != nil {
			return nil, err
		}
		pages = append(pages, next.TimelineItems.Nodes)
		page = next.TimelineItems.PageInfo
	}
	var items []githubTimelineItemRaw
	for i := len(pages) - 1; i >= 0; i-- {
		items = append(items, pages[i]...)
	}
	return githubReviewEvents(items), nil
}

// withAfter copies vars with the page cursor set.
func withAfter(vars map[string]any, after string) map[string]any {
	out := make(map[string]any, len(vars)+1)
	for k, v := range vars {
		out[k] = v
	}
	out["after"] = after
	return out
}

func githubPRDetail(pull *githubPullRaw, viewer string, checks []CheckStatus) PRDetail {
	return PRDetail{
		Number:         pull.Number,
		Title:          pull.Title,
		Body:           pull.Body,
		AuthorLogin:    pull.Author.Login,
		AuthorName:     pull.Author.Name,
		State:          NormalizePRState(pull.State),
		Draft:          pull.IsDraft,
		HeadRefName:    pull.HeadRefName,
		BaseRefName:    pull.BaseRefName,
		BaseRefSHA:     pull.BaseRefOID,
		HeadSHA:        pull.HeadRefOID,
		URL:            pull.URL,
		Additions:      pull.Additions,
		Deletions:      pull.Deletions,
		ChangedFiles:   pull.ChangedFiles,
		ViewerIsAuthor: viewer != "" && strings.EqualFold(viewer, pull.Author.Login),
		ReviewDecision: pull.ReviewDecision,
		LatestReviews:  githubLatestReviews(pull.LatestReviews.Nodes),
		Checks:         githubCheckSummary(checks),
		Mergeability:   normalizeGitHubMergeability(pull.Mergeable, pull.MergeStateStatus),
	}
}

// githubLatestReviews maps latestReviews, GitHub's own latest review per
// author, skipping an entry with no author or state.
func githubLatestReviews(reviews []githubReviewRaw) []ReviewVerdict {
	out := make([]ReviewVerdict, 0, len(reviews))
	for _, review := range reviews {
		if review.Author.Login == "" || review.State == "" {
			continue
		}
		out = append(out, ReviewVerdict{
			AuthorLogin: review.Author.Login,
			AuthorName:  review.Author.Name,
			State:       review.State,
			SubmittedAt: review.SubmittedAt,
			Body:        review.Body,
			CommitSHA:   review.Commit.OID,
		})
	}
	return out
}

// githubCheckStatuses maps rollup contexts to checks, skipping a context
// of another type.
func githubCheckStatuses(contexts []githubContextRaw) []CheckStatus {
	out := make([]CheckStatus, 0, len(contexts))
	for _, item := range contexts {
		if check, ok := githubCheckStatus(item); ok {
			out = append(out, check)
		}
	}
	return out
}

// githubCheckStatus maps one rollup context. A CheckRun's workflow is its
// check suite's workflow run's; a StatusContext's start is its creation.
// ok is false for a context of another type.
func githubCheckStatus(item githubContextRaw) (CheckStatus, bool) {
	check := CheckStatus{Kind: item.Typename}
	switch item.Typename {
	case "CheckRun":
		if item.Name == "" {
			// A read without check runs ($wantCheckRuns) answers only
			// their __typename.
			return CheckStatus{}, false
		}
		check.Name = item.Name
		if suite := item.CheckSuite; suite != nil && suite.WorkflowRun != nil && suite.WorkflowRun.Workflow != nil {
			check.Workflow = suite.WorkflowRun.Workflow.Name
		}
		check.Status = item.Status
		check.Conclusion = item.Conclusion
		check.DetailsURL = item.DetailsURL
		check.StartedAt = item.StartedAt
		check.CompletedAt = item.CompletedAt
	case "StatusContext":
		check.Name = item.Context
		check.Status = item.State
		check.DetailsURL = item.TargetURL
		check.StartedAt = item.CreatedAt
	default:
		return CheckStatus{}, false
	}
	return check, true
}

func githubCheckSummary(checks []CheckStatus) CheckSummary {
	summary := CheckSummary{Checks: checks}
	for _, check := range checks {
		summary.Total++
		addCheckBucket(&summary, check)
	}
	return summary
}

func addCheckBucket(summary *CheckSummary, check CheckStatus) {
	switch strings.ToUpper(firstNonEmpty(check.Conclusion, check.Status)) {
	case "SUCCESS":
		summary.Success++
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		summary.Failure++
	case "CANCELLED", "CANCELED":
		summary.Canceled++
	case "SKIPPED", "NEUTRAL":
		summary.Skipped++
	default:
		summary.Pending++
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func normalizeGitHubMergeability(mergeable, mergeStateStatus string) string {
	switch strings.ToUpper(strings.TrimSpace(mergeable)) {
	case "CONFLICTING":
		return MergeabilityConflicts
	case "UNKNOWN", "":
		return MergeabilityChecking
	case "MERGEABLE":
		if strings.EqualFold(mergeStateStatus, "DIRTY") {
			return MergeabilityConflicts
		}
		return MergeabilityClean
	default:
		return MergeabilityChecking
	}
}

// Why a thread's Resolve and Reply are unavailable. GitHub lets the pull
// request's author and people with write access resolve; a reply is
// refused on a locked conversation or without read access.
const (
	githubCannotResolveReason = "Only the pull request's author and people with write access can resolve or reopen conversations."
	githubCannotReplyReason   = "GitHub does not let you reply to this conversation."
)

func githubReviewThreads(nodes []githubThreadRaw) []ReviewThread {
	threads := make([]ReviewThread, 0, len(nodes))
	for _, node := range nodes {
		side := strings.ToLower(node.DiffSide)
		if strings.EqualFold(node.SubjectType, "FILE") {
			side = "file"
		}
		thread := ReviewThread{
			ID:                node.ID,
			Path:              node.Path,
			Line:              node.Line,
			StartLine:         node.StartLine,
			OriginalLine:      node.OriginalLine,
			OriginalStartLine: node.OriginalStartLine,
			Side:              side,
			StartSide:         strings.ToLower(node.StartDiffSide),
			IsResolvable:      true,
			IsResolved:        node.IsResolved,
			IsOutdated:        node.IsOutdated,
			Comments:          make([]ReviewComment, 0, len(node.Comments.Nodes)),
		}
		// GitHub answers both directions; the one shown is the one the
		// thread's state offers.
		may := node.ViewerCanResolve
		if node.IsResolved {
			may = node.ViewerCanUnresolve
		}
		if !may {
			thread.CannotResolveReason = githubCannotResolveReason
		}
		if !node.ViewerCanReply {
			thread.CannotReplyReason = githubCannotReplyReason
		}
		for _, comment := range node.Comments.Nodes {
			out := ReviewComment{
				ID:          comment.ID,
				AuthorLogin: comment.Author.Login,
				AuthorName:  comment.Author.Name,
				Body:        comment.Body,
				CreatedAt:   comment.CreatedAt,
				DatabaseID:  int64(comment.DatabaseID),
			}
			comment.fill(&out)
			if comment.ReplyTo != nil {
				out.ReplyTo = &ReviewReplyTo{ID: comment.ReplyTo.ID, DatabaseID: int64(comment.ReplyTo.DatabaseID)}
			}
			thread.Comments = append(thread.Comments, out)
		}
		threads = append(threads, thread)
	}
	return threads
}

// githubConversationThreads maps conversation comments to path-less
// single-comment threads, skipping minimized ones.
func githubConversationThreads(nodes []githubCommentRaw) []ReviewThread {
	threads := make([]ReviewThread, 0, len(nodes))
	for _, node := range nodes {
		if node.IsMinimized {
			continue
		}
		comment := ReviewComment{
			ID:          node.ID,
			AuthorLogin: node.Author.Login,
			AuthorName:  node.Author.Name,
			Body:        node.Body,
			CreatedAt:   node.CreatedAt,
			DatabaseID:  int64(node.DatabaseID),
		}
		node.fill(&comment)
		// The thread is named by its one comment (CommentTarget).
		threads = append(threads, ReviewThread{ID: node.ID, Comments: []ReviewComment{comment}})
	}
	return threads
}

// fill copies the viewer's permissions, the edit time, the URL and the
// reactions onto out.
func (raw githubViewerCommentRaw) fill(out *ReviewComment) {
	out.EditedAt = raw.LastEditedAt
	out.URL = raw.URL
	out.ViewerCanEdit = raw.ViewerCanUpdate
	out.ViewerCanDelete = raw.ViewerCanDelete
	if !raw.ViewerCanReact {
		out.CannotReactReason = githubCannotReactReason
	}
	out.Reactions = githubReactions(raw.ReactionGroups)
}

// githubCannotReactReason is why a comment takes no reaction from the
// viewer: GitHub's viewerCanReact is false on a locked conversation and
// in an archived repository.
const githubCannotReactReason = "GitHub does not let you react to this comment."
