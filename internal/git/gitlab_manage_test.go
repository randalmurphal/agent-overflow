package git

import (
	"net/http"
	"strings"
)

// gitlabManageEmpty is a PRManage answer with no people or labels and no
// permissions.
const gitlabManageEmpty = `{"data":{"project":{"mergeRequest":{"userPermissions":{"updateMergeRequest":false,"adminMergeRequest":false},` +
	`"reviewers":{"nodes":[]},"assignees":{"nodes":[]},"labels":{"nodes":[]}}}}}`

// gitlabNoReactions is a MRNoteReactions answer with no notes.
const gitlabNoReactions = `{"data":{"project":{"mergeRequest":{"notes":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}`

// gitlabManageExtras answers the reads a GitLab PR read adds for the
// management actions, for a test about something else: the detail's
// release (19.3) and PRManageChecks (gitlabManageEmpty), the threads'
// reactions (none) and the events' state events (none). ok is false for
// any other request.
func gitlabManageExtras(call forgeAPICall) (forgeAPIAnswer, bool) {
	path, _, _ := strings.Cut(call.Path, "?")
	switch {
	case call.Op == "" && call.Method == http.MethodGet && path == "metadata":
		return forgeAPIAnswer{Body: `{"version":"19.3.0-ee","enterprise":true}`}, true
	case call.Op == "PRManageChecks":
		return forgeAPIAnswer{Body: gitlabManageEmpty}, true
	case call.Op == "MRNoteReactions":
		return forgeAPIAnswer{Body: gitlabNoReactions}, true
	case call.Op == "" && call.Method == http.MethodGet && strings.HasSuffix(path, "/resource_state_events"):
		return forgeAPIAnswer{Body: `[]`}, true
	}
	return forgeAPIAnswer{}, false
}
