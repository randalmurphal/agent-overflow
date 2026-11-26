package trackerfake

// The Jira roots: the platform API, and Jira Software's.
const (
	api      = "rest/api/3/"
	agile    = "rest/agile/1.0/"
	software = "rest/software/1.0/"
)

// Query parameter sets shared by routes.
var (
	window      = []string{"startAt", "maxResults"}
	listQuery   = []string{"jql", "fields", "maxResults", "nextPageToken"}
	adjustQuery = []string{"adjustEstimate", "newEstimate", "reduceBy", "increaseBy"}
)

func with(base []string, more ...string) []string {
	return append(append([]string{}, base...), more...)
}

// jiraRoutes are every Jira route the fake implements. An issue-scoped
// read answers an anonymous request as not found, as Jira does.
var jiraRoutes = []jiraRoute{
	{method: "GET", pattern: api + "myself", reads: true, handle: myself},

	// Planning poker (jira_poker.go).
	{method: "GET", pattern: "_edge/tenant_info", reads: true, handle: tenantInfo},
	{method: "POST", pattern: "gateway/api/graphql", readsWhen: gatewayReads, handle: gateway},
	{method: "POST", pattern: "rest/internal/2/forge/context/token", handle: contextToken},

	{method: "GET", pattern: api + "user", query: []string{"accountId"}, reads: true, handle: getUser},
	{method: "GET", pattern: api + "user/search", query: with(window, "query", "accountId"), reads: true, handle: searchUsers},
	{method: "GET", pattern: api + "user/assignable/search", query: with(window, "query", "accountId", "issueKey", "issueId", "project"), reads: true, handle: assignableUsers},
	{method: "GET", pattern: api + "field", reads: true, handle: listFields},
	{method: "GET", pattern: api + "issue/createmeta/{project}/issuetypes", query: window, reads: true, handle: createMetaTypes},
	{method: "GET", pattern: api + "issue/createmeta/{project}/issuetypes/{type}", query: window, reads: true, handle: createMetaFields},
	{method: "GET", pattern: api + "project/search", query: with(window, "orderBy", "query"), reads: true, handle: searchProjects},
	{method: "GET", pattern: api + "project/{project}", query: []string{"expand"}, reads: true, handle: getProject},
	{method: "GET", pattern: api + "project/{project}/statuses", reads: true, handle: projectStatuses},
	{method: "GET", pattern: api + "status", reads: true, handle: listStatuses},
	{method: "GET", pattern: api + "priority/search", query: window, reads: true, handle: searchPriorities},
	{method: "GET", pattern: api + "issueLinkType", reads: true, handle: listLinkTypes},

	{method: "POST", pattern: api + "issue", handle: createIssue},
	{method: "GET", pattern: api + "issue/{issue}", query: []string{"fields", "expand"}, repeat: []string{"fields"}, anonymous404: true, reads: true, handle: getIssue},
	{method: "PUT", pattern: api + "issue/{issue}", query: []string{"returnIssue", "fields"}, handle: editIssue},
	{method: "GET", pattern: api + "issue/{issue}/editmeta", anonymous404: true, reads: true, handle: editMeta},
	{method: "PUT", pattern: api + "issue/{issue}/assignee", handle: assignIssue},
	{method: "GET", pattern: api + "issue/{issue}/transitions", query: []string{"expand"}, anonymous404: true, reads: true, handle: getTransitions},
	{method: "POST", pattern: api + "issue/{issue}/transitions", handle: transitionIssue},
	{method: "POST", pattern: api + "search/jql", reads: true, handle: searchJQL},
	{method: "POST", pattern: api + "search/approximate-count", reads: true, handle: countJQL},
	{method: "POST", pattern: api + "issue/bulkfetch", reads: true, handle: bulkFetch},
	{method: "POST", pattern: api + "jql/match", reads: true, handle: matchJQL},
	{method: "GET", pattern: api + "issue/{issue}/changelog", query: window, anonymous404: true, reads: true, handle: issueChangelog},
	{method: "POST", pattern: api + "changelog/bulkfetch", reads: true, handle: bulkChangelog},

	{method: "GET", pattern: api + "issue/{issue}/comment", query: with(window, "orderBy"), anonymous404: true, reads: true, handle: listComments},
	{method: "POST", pattern: api + "issue/{issue}/comment", handle: addComment},
	{method: "GET", pattern: api + "issue/{issue}/comment/{id}", anonymous404: true, reads: true, handle: getComment},
	{method: "PUT", pattern: api + "issue/{issue}/comment/{id}", handle: editComment},
	{method: "DELETE", pattern: api + "issue/{issue}/comment/{id}", handle: deleteComment},
	{method: "POST", pattern: api + "issueLink", handle: linkIssues},
	{method: "DELETE", pattern: api + "issueLink/{id}", handle: unlinkIssues},
	{method: "GET", pattern: api + "issue/{issue}/remotelink", anonymous404: true, reads: true, handle: listRemoteLinks},
	{method: "POST", pattern: api + "issue/{issue}/remotelink", handle: addRemoteLink},
	{method: "DELETE", pattern: api + "issue/{issue}/remotelink/{id}", handle: removeRemoteLink},
	{method: "GET", pattern: api + "issue/{issue}/watchers", anonymous404: true, reads: true, handle: getWatchers},
	{method: "POST", pattern: api + "issue/{issue}/watchers", handle: addWatcher},
	{method: "DELETE", pattern: api + "issue/{issue}/watchers", query: []string{"accountId"}, handle: removeWatcher},
	{method: "POST", pattern: api + "issue/{issue}/attachments", handle: attach},
	{method: "DELETE", pattern: api + "attachment/{id}", handle: deleteAttachment},
	{method: "GET", pattern: api + "attachment/content/{id}", query: []string{"redirect"}, reads: true, handle: attachmentContent},

	{method: "GET", pattern: api + "issue/{issue}/worklog", query: with(window, "startedAfter", "startedBefore"), anonymous404: true, reads: true, handle: listWorklogs},
	{method: "GET", pattern: api + "issue/{issue}/worklog/{id}", anonymous404: true, reads: true, handle: getWorklog},
	{method: "POST", pattern: api + "issue/{issue}/worklog", query: with(adjustQuery), handle: addWorklog},
	{method: "PUT", pattern: api + "issue/{issue}/worklog/{id}", query: with(adjustQuery), handle: updateWorklog},
	{method: "DELETE", pattern: api + "issue/{issue}/worklog/{id}", query: with(adjustQuery), handle: deleteWorklog},
	{method: "GET", pattern: api + "worklog/updated", query: []string{"since"}, reads: true, handle: updatedWorklogs},
	{method: "GET", pattern: api + "worklog/deleted", query: []string{"since"}, reads: true, handle: deletedWorklogs},
	{method: "POST", pattern: api + "worklog/list", reads: true, handle: worklogsByID},

	{method: "GET", pattern: agile + "board", query: with(window, "projectKeyOrId", "type", "name"), reads: true, handle: listBoards},
	{method: "GET", pattern: agile + "board/{board}", reads: true, handle: getBoard},
	{method: "GET", pattern: agile + "board/{board}/configuration", reads: true, handle: boardConfig},
	{method: "GET", pattern: agile + "board/{board}/project", query: window, reads: true, handle: boardProjects},
	{method: "GET", pattern: agile + "board/{board}/sprint", query: with(window, "state"), reads: true, handle: boardSprints},
	{method: "GET", pattern: agile + "sprint/{sprint}", reads: true, handle: getSprint},
	{method: "POST", pattern: agile + "sprint", handle: createSprint},
	{method: "POST", pattern: agile + "sprint/{sprint}", handle: updateSprint},
	{method: "DELETE", pattern: agile + "sprint/{sprint}", handle: deleteSprint},
	{method: "POST", pattern: agile + "sprint/{sprint}/issue", handle: moveToSprint},
	{method: "POST", pattern: agile + "backlog/issue", handle: moveToBacklog},
	{method: "POST", pattern: agile + "backlog/{board}/issue", handle: moveToBacklog},
	{method: "PUT", pattern: agile + "issue/rank", handle: rankIssues},
	{method: "PUT", pattern: agile + "issue/{issue}/estimation", query: []string{"boardId"}, handle: setEstimation},

	{method: "GET", pattern: software + "board/{board}/issue", query: listQuery, repeat: []string{"fields"}, reads: true, handle: softwareList(false, true)},
	{method: "GET", pattern: software + "board/{board}/backlog", query: listQuery, repeat: []string{"fields"}, reads: true, handle: softwareList(true, false)},
	{method: "GET", pattern: software + "sprint/{sprint}/issue", query: listQuery, repeat: []string{"fields"}, reads: true, handle: softwareList(false, false)},
	{method: "GET", pattern: software + "board/{board}/sprint/{sprint}/issue", query: listQuery, repeat: []string{"fields"}, reads: true, handle: softwareList(false, false)},
	{method: "GET", pattern: software + "epic/{epic}/issue", query: listQuery, repeat: []string{"fields"}, reads: true, handle: softwareList(false, false)},
	{method: "GET", pattern: software + "board/{board}/issue/approximate-count", query: []string{"jql"}, reads: true, handle: softwareCount(false)},
	{method: "GET", pattern: software + "board/{board}/backlog/approximate-count", query: []string{"jql"}, reads: true, handle: softwareCount(true)},
}
