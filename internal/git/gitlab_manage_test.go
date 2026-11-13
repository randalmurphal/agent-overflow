package git

import (
	"net/http"
	"strings"
)

// gitlabManageEmpty is a PRManage answer with no people or labels, no
// permissions and a server without the rereview and override features.
const gitlabManageEmpty = `{"data":{"project":{"mergeRequest":{"userPermissions":{"updateMergeRequest":false,"adminMergeRequest":false},` +
	`"reviewers":{"nodes":[]},"assignees":{"nodes":[]},"labels":{"nodes":[]}}},"rereview":null,"updateInput":{"inputFields":[]}}}`

// gitlabManageExtras answers the reads a GitLab PR read adds for the
// management actions, for a test about something else: the detail's
// PRManage (gitlabManageEmpty) and the events' state events (none). ok is
// false for any other request.
func gitlabManageExtras(call forgeAPICall) (forgeAPIAnswer, bool) {
	path, _, _ := strings.Cut(call.Path, "?")
	switch {
	case call.Op == "PRManage":
		return forgeAPIAnswer{Body: gitlabManageEmpty}, true
	case call.Op == "" && call.Method == http.MethodGet && strings.HasSuffix(path, "/resource_state_events"):
		return forgeAPIAnswer{Body: `[]`}, true
	}
	return forgeAPIAnswer{}, false
}
