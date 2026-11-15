package git

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"
)

// The shape of the GitHub PR read: which optional parts it selects and how
// much it asks for per page. GitHub answers a selection the token may not
// read (a team without read:org, a check run or a workflow run a
// fine-grained token cannot read, a field this server lacks) by failing
// the whole document or nulling a parent, so each such selection is an
// optional part behind its own @include variable: a read refused for a
// part is sent again without it, the part's sentence says what is not
// shown and why, and the refusal is remembered for the repository and
// login for githubReadShapeTTL so the next ticks do not pay for it. A PR
// too large to answer (a body past its bound, or GitHub's 10 second
// query limit) steps down githubReadLevels the same way, remembered for
// that PR.

// githubReadPart is an optional part of the read: the @include variable
// that selects it.
type githubReadPart string

const (
	githubPartManage       githubReadPart = "wantManage"
	githubPartTeams        githubReadPart = "wantTeams"
	githubPartCheckRuns    githubReadPart = "wantCheckRuns"
	githubPartWorkflowRuns githubReadPart = "wantWorkflowRuns"
)

// githubReadParts are the optional parts in the order their sentences
// show.
var githubReadParts = []githubReadPart{githubPartManage, githubPartTeams, githubPartCheckRuns, githubPartWorkflowRuns}

// githubPartWhat opens a part's sentence.
var githubPartWhat = map[githubReadPart]string{
	githubPartManage:       "Reviewers, assignees, labels and the PR's edit controls are",
	githubPartTeams:        "Team reviewers are",
	githubPartCheckRuns:    "Check runs are",
	githubPartWorkflowRuns: "Workflow runs (each check's workflow, log and re-run) are",
}

// githubPartFields name each part in a refusal that has no path (GitHub
// refuses a document before running it with a message naming the
// field): fields only that part selects.
var githubPartFields = map[githubReadPart][]string{
	githubPartManage:       {"viewerCanUpdate", "viewerCanClose", "viewerCanReopen", "viewerCanLabel", "viewerCanAssign", "reviewRequests", "assignees", "labels"},
	githubPartTeams:        {"combinedSlug", "Team"},
	githubPartCheckRuns:    {"CheckRun", "conclusion", "detailsUrl"},
	githubPartWorkflowRuns: {"checkSuite", "workflowRun"},
}

// appliesTo reports whether want selects the part at all.
func (p githubReadPart) appliesTo(want PRReadParts) bool {
	switch p {
	case githubPartManage:
		return want.Detail
	case githubPartTeams:
		return want.Detail || want.Events
	}
	return want.Detail || want.CI
}

// githubReadLevels are the read's sizes, largest first: each threads read
// that is too large to answer steps down one. The second drops the
// comments' reaction groups (all eight of every comment); the third also
// reads threads and comments 25 per page, so no one request carries much.
var githubReadLevels = []struct {
	reactions bool
	pageSize  int
}{{true, githubReadPageSize}, {false, githubReadPageSize}, {false, 25}}

// githubReadShapeTTL is how long a refused part or a smaller level is
// kept before the full read is tried again.
const githubReadShapeTTL = 10 * time.Minute

// githubReadShape is one read's shape: its level and the parts it leaves
// out, each with its sentence.
type githubReadShape struct {
	level    int
	excluded map[githubReadPart]string
}

func (s githubReadShape) reactions() bool { return githubReadLevels[s.level].reactions }
func (s githubReadShape) pageSize() int   { return githubReadLevels[s.level].pageSize }

// has reports whether the read selects part.
func (s githubReadShape) has(part githubReadPart) bool {
	_, out := s.excluded[part]
	return !out
}

// setVars sets the parts' variables a document declares.
func (s githubReadShape) setVars(vars map[string]any, parts ...githubReadPart) {
	for _, part := range parts {
		vars[string(part)] = s.has(part)
	}
}

// unavailable is the sentences of the parts the read left out, but the
// management part's, which PRManage.Unavailable carries.
func (s githubReadShape) unavailable() []string {
	var out []string
	for _, part := range githubReadParts {
		if sentence, ok := s.excluded[part]; ok && part != githubPartManage {
			out = append(out, sentence)
		}
	}
	return out
}

// ciUnavailable is the sentences of the left-out parts a pipeline is
// built from (check runs, workflow runs), one string.
func (s githubReadShape) ciUnavailable() string {
	var out []string
	for _, part := range []githubReadPart{githubPartCheckRuns, githubPartWorkflowRuns} {
		if sentence, ok := s.excluded[part]; ok {
			out = append(out, sentence)
		}
	}
	return strings.Join(out, " ")
}

// githubPartsOf attributes a refused read to the optional parts it may
// leave out. A problem with a path is the part under it, or the core of
// the read; one without (a document GitHub refused before running it) is
// the part its message names. ok is false when the read cannot be
// helped by leaving parts out: a problem in the core, an error that is
// not a GraphQL answer, a single sign-on refusal, or nothing left to
// leave out. A refusal no problem attributes is answered by leaving out
// every part still selected. remember is whether the refusal is a fact
// about the token or the server (FORBIDDEN, INSUFFICIENT_SCOPES, a
// document refused before it ran), not an answer the next read may not
// repeat.
func githubPartsOf(err error, want PRReadParts, shape githubReadShape) (parts []githubReadPart, remember, ok bool) {
	gql, isGQL := errors.AsType[*forgeapi.GraphQLError](err)
	if !isGQL || gql.SSO || len(gql.Problems) == 0 {
		return nil, false, false
	}
	refusedEarly := len(gql.Data) == 0
	unattributed := false
	for _, problem := range gql.Problems {
		candidates := githubPartOf(problem)
		i := slices.IndexFunc(candidates, func(part githubReadPart) bool { return shape.has(part) && part.appliesTo(want) })
		switch {
		case i >= 0:
			if !slices.Contains(parts, candidates[i]) {
				parts = append(parts, candidates[i])
			}
		case len(problem.Path) > 0:
			return nil, false, false
		default:
			unattributed = true
		}
	}
	if unattributed {
		if !refusedEarly && !gql.Forbidden() {
			return nil, false, false
		}
		parts = parts[:0]
		for _, part := range githubReadParts {
			if shape.has(part) && part.appliesTo(want) {
				parts = append(parts, part)
			}
		}
	}
	return parts, refusedEarly || gql.Forbidden(), len(parts) > 0
}

// githubPartOf is the parts a problem may lie in, the narrowest first: a
// problem under a review request's reviewer is its team's, and with
// teams left out the management part's; one under a check suite is its
// workflow run's, then its check run's.
func githubPartOf(problem forgeapi.GraphQLProblem) []githubReadPart {
	if len(problem.Path) > 0 {
		path := make([]string, 0, len(problem.Path))
		for _, segment := range problem.Path {
			if name, ok := segment.(string); ok {
				path = append(path, name)
			}
		}
		var out []githubReadPart
		switch {
		case slices.Contains(path, "checkSuite") || slices.Contains(path, "workflowRun"):
			out = append(out, githubPartWorkflowRuns, githubPartCheckRuns)
		case slices.Contains(path, "statusCheckRollup"):
			out = append(out, githubPartCheckRuns)
		case slices.Contains(path, "requestedReviewer"):
			out = append(out, githubPartTeams)
		}
		if len(path) >= 3 && path[0] == "repository" && path[1] == "pullRequest" && githubManageSelections[path[2]] {
			out = append(out, githubPartManage)
		}
		return out
	}
	for _, part := range githubReadParts {
		for _, field := range githubPartFields[part] {
			if strings.Contains(problem.Message, "'"+field+"'") || strings.Contains(problem.Message, "'"+field+".") || strings.Contains(problem.Message, "."+field+"'") {
				return []githubReadPart{part}
			}
		}
	}
	return nil
}

// githubPartSentence says what a left-out part does not show and why.
func githubPartSentence(part githubReadPart, ref PRReference, err error) string {
	return githubRefusedSentence(githubPartWhat[part], ref.Forge, ref.Host, err)
}

// githubRefusedSentence says that what is not shown, refused as err
// says, on forge's host.
func githubRefusedSentence(what, forge, hostName string, err error) string {
	labels := forgeLabels(forge)
	host := labels.host(hostName)
	gql, _ := errors.AsType[*forgeapi.GraphQLError](err)
	message := ""
	if gql != nil && len(gql.Problems) > 0 {
		message = gql.Problems[0].Message
	}
	switch {
	case gql != nil && gql.MissingScope() != "":
		return fmt.Sprintf("%s not shown: your %s token lacks the `%s` scope. Create a token with that scope and log %s in with it.", what, host, gql.MissingScope(), labels.binary)
	case gql != nil && gql.FineGrained():
		return fmt.Sprintf("%s not shown: %s does not let this fine-grained token read them.", what, host)
	case strings.Contains(message, "doesn't exist on type"):
		return fmt.Sprintf("%s not shown: %s does not have them.", what, host)
	case message != "":
		return fmt.Sprintf("%s not shown: %s refused them (%s).", what, host, trimSentence(message))
	}
	return fmt.Sprintf("%s not shown: %s refused them.", what, host)
}

// githubTooLarge reports a read too large to answer: a body past its
// bound, or GitHub's answer to a query it stopped after 10 seconds ("you
// may receive either a 502 or 504 status code", "We couldn't respond to
// your request in time"; its GraphQL rate limits reference).
func githubTooLarge(err error) bool {
	if errors.Is(err, forgeapi.ErrBodyTooLarge) {
		return true
	}
	if status, ok := errors.AsType[*forgeapi.StatusError](err); ok {
		return status.Status == http.StatusBadGateway || status.Status == http.StatusGatewayTimeout
	}
	if gql, ok := errors.AsType[*forgeapi.GraphQLError](err); ok {
		return slices.ContainsFunc(gql.Problems, func(p forgeapi.GraphQLProblem) bool {
			lower := strings.ToLower(p.Message)
			return strings.Contains(lower, "couldn't respond to your request in time") || strings.Contains(lower, "timeout")
		})
	}
	return false
}

// githubReadMemory is what the reads remember of their shape: the parts
// a repository's reads leave out for a login, and the level a PR needs.
type githubReadMemory struct {
	parts  ciCapabilityCache[map[githubReadPart]string]
	levels ciCapabilityCache[int]
}

func (m *githubReadMemory) clear() {
	m.parts.clear()
	m.levels.clear()
}

// shape is the remembered shape of a read: the parts left out for key's
// repository and login, and for a threads read the PR's level.
func (m *githubReadMemory) shape(partsKey, levelKey string, threads bool, now time.Time) githubReadShape {
	shape := githubReadShape{excluded: map[githubReadPart]string{}}
	if parts, _, ok := m.parts.get(partsKey, now); ok {
		maps.Copy(shape.excluded, parts)
	}
	if level, _, ok := m.levels.get(levelKey, now); ok && threads {
		shape.level = level
	}
	return shape
}
