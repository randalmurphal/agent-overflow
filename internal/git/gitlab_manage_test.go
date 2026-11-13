package git

import (
	"net/http"
	"strings"
)

// gitlabManageEmpty is a PRManage answer with no people or labels and no
// permissions.
const gitlabManageEmpty = `{"data":{"project":{"mergeRequest":{"userPermissions":{"updateMergeRequest":false,"adminMergeRequest":false},` +
	`"reviewers":{"nodes":[]},"assignees":{"nodes":[]},"labels":{"nodes":[]}}}}}`

// gitlabSchemaBare is a PRManageSchema answer of a server with none of
// the management features.
const gitlabSchemaBare = `{"data":{"rereview":null,"updateInput":{"inputFields":[]},"setReviewers":null,"setAssignees":null,"setLabels":null}}`

// gitlabManageExtras answers the reads a GitLab PR read adds for the
// management actions, for a test about something else: the detail's
// PRManage (gitlabManageEmpty) and the events' state events (none). ok is
// false for any other request.
// gitlabNoReactions is a MRNoteReactions answer with no notes.
const gitlabNoReactions = `{"data":{"project":{"mergeRequest":{"notes":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}`

func gitlabManageExtras(call forgeAPICall) (forgeAPIAnswer, bool) {
	path, _, _ := strings.Cut(call.Path, "?")
	switch {
	case call.Op == "PRManageSchema":
		return forgeAPIAnswer{Body: gitlabSchemaBare}, true
	case call.Op == "PRManage":
		return forgeAPIAnswer{Body: gitlabManageEmpty}, true
	case call.Op == "MRNoteReactions":
		return forgeAPIAnswer{Body: gitlabNoReactions}, true
	case call.Op == "" && call.Method == http.MethodGet && strings.HasSuffix(path, "/resource_state_events"):
		return forgeAPIAnswer{Body: `[]`}, true
	}
	return forgeAPIAnswer{}, false
}
