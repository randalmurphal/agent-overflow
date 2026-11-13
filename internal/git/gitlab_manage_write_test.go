package git

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// gitlabSchemaAll is a PRManageSchema answer of a server with every
// management feature.
const gitlabSchemaAll = `{"data":{"rereview":{"name":"x"},"updateInput":{"inputFields":[{"name":"overrideRequestedChanges"}]},` +
	`"setReviewers":{"inputFields":[{"name":"operationMode"}]},"setAssignees":{"inputFields":[{"name":"operationMode"}]},"setLabels":{"inputFields":[{"name":"operationMode"}]}}}`

// gitlabReviewersAnswer is a mergeRequestSetReviewers answer listing
// usernames as the merge request's reviewers.
func gitlabReviewersAnswer(usernames ...string) string {
	nodes := make([]string, 0, len(usernames))
	for _, name := range usernames {
		nodes = append(nodes, `{"username":"`+name+`"}`)
	}
	return `{"mergeRequestSetReviewers":{"errors":[],"mergeRequest":{"reviewers":{"nodes":[` + strings.Join(nodes, ",") + `]}}}}`
}

// A GitLab people change is a REMOVE, then an APPEND, each checked
// against the merge request GitLab answers. A failure of the first is the
// write's own; one after it is a partial change: refused (payload errors,
// a person GitLab did not add) or unconfirmed (no answer, or data beside
// errors), never advising a retry when it may have applied.
func TestGitLabPeopleChangeWritesInOrder(t *testing.T) {
	t.Parallel()
	removed := forgeAPIAnswer{Body: `{"data":` + gitlabReviewersAnswer("dave") + `}`}
	for _, tc := range []struct {
		name          string
		first, second forgeAPIAnswer
		writes        int
		want          string
		partial       bool
		refused       bool
	}{
		{name: "both applied", first: removed, second: forgeAPIAnswer{Body: `{"data":` + gitlabReviewersAnswer("dave", "bob") + `}`}, writes: 2},
		{name: "first unanswered", first: forgeAPIAnswer{Status: http.StatusBadGateway}, writes: 1,
			want: "GitLab did not confirm the request to change the reviewers of grp/sub/app!7, so it may have been applied. The pane shows what GitLab did."},
		{name: "first kept someone", first: forgeAPIAnswer{Body: `{"data":` + gitlabReviewersAnswer("dave", "carol") + `}`}, writes: 1, refused: true,
			want: "GitLab kept carol on the reviewers."},
		{name: "second unanswered", first: removed, second: forgeAPIAnswer{Status: http.StatusBadGateway}, writes: 2, partial: true,
			want: "Removed carol from the reviewers of grp/sub/app!7; adding bob may have applied. The pane shows what GitLab did."},
		{name: "second answered with data and errors", first: removed, writes: 2, partial: true,
			second: forgeAPIAnswer{Body: `{"data":` + gitlabReviewersAnswer("dave", "bob") + `,"errors":[{"message":"Internal server error"}]}`},
			want:   "Removed carol from the reviewers of grp/sub/app!7; adding bob may have applied."},
		{name: "second refused in the payload", first: removed, writes: 2, partial: true, refused: true,
			second: forgeAPIAnswer{Body: `{"data":{"mergeRequestSetReviewers":{"errors":["Reviewers are invalid"],"mergeRequest":null}}}`},
			want:   "Removed carol from the reviewers of grp/sub/app!7; adding bob was refused: "},
		{name: "second did not add", first: removed, second: removed, writes: 2, partial: true, refused: true,
			want: "Removed carol from the reviewers of grp/sub/app!7; adding bob was refused: GitLab did not add bob to the reviewers: they may not be members of the project."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				switch {
				case call.Op == "PRManageSchema":
					return forgeAPIAnswer{Body: gitlabSchemaAll}
				case call.Op == "PRSetReviewers" && call.Vars["mode"] == "REMOVE":
					return tc.first
				case call.Op == "PRSetReviewers" && call.Vars["mode"] == "APPEND":
					return tc.second
				}
				return forgeUnexpected(t, call)
			})
			ref := gitlabTestRef(7)
			err := core.SetPRPeople(t.Context(), ref, true, PRPeopleChange{Add: []PRPerson{{Login: "bob"}}, Remove: []PRPerson{{Login: "carol"}}})
			var writes []string
			for _, call := range calls.all() {
				if call.Op == "PRSetReviewers" {
					writes = append(writes, call.Vars["mode"].(string)+" "+strings.Join(anyStringsOf(call.Vars["usernames"]), ","))
				}
			}
			if want := []string{"REMOVE carol", "APPEND bob"}[:tc.writes]; !slices.Equal(writes, want) {
				t.Fatalf("writes = %q, want %q", writes, want)
			}
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			_, partial := errors.AsType[*PartialWriteError](err)
			explained, _ := errors.AsType[*ForgeActionError](ExplainForgeError(ref, ActionReviewers, err))
			if partial != tc.partial || explained == nil || !strings.HasPrefix(explained.Message, tc.want) || (explained.Kind == ForgeErrorTransient) == tc.refused {
				t.Fatalf("err = %v, explained %+v; want partial %v, refused %v, %q", err, explained, tc.partial, tc.refused, tc.want)
			}
			if !tc.refused && strings.Contains(strings.ToLower(explained.Message), "try again") {
				t.Fatalf("an unconfirmed write advises a retry: %q", explained.Message)
			}
		})
	}
}

// Labels are removed, then added, by id; a host without the set
// mutations' operationMode refuses the change before any write.
func TestGitLabLabelChangeAndUnsupportedHost(t *testing.T) {
	t.Parallel()
	bug, ui := PRLabel{ID: "gid://gitlab/ProjectLabel/1", Name: "bug"}, PRLabel{ID: "gid://gitlab/ProjectLabel/2", Name: "ui,web"}
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Op == "PRManageSchema":
			return forgeAPIAnswer{Body: gitlabSchemaAll}
		case call.Op == "PRSetLabels" && call.Vars["mode"] == "REMOVE":
			return forgeAPIAnswer{Body: `{"data":{"mergeRequestSetLabels":{"errors":[],"mergeRequest":{"labels":{"nodes":[]}}}}}`}
		case call.Op == "PRSetLabels":
			return forgeAPIAnswer{Body: `{"data":{"mergeRequestSetLabels":{"errors":[],"mergeRequest":{"labels":{"nodes":[{"id":"` + ui.ID + `"}]}}}}}`}
		}
		return forgeUnexpected(t, call)
	})
	if err := core.SetPRLabels(t.Context(), gitlabTestRef(7), PRLabelChange{Add: []PRLabel{ui}, Remove: []PRLabel{bug}}); err != nil {
		t.Fatal(err)
	}
	var writes []string
	for _, call := range calls.all() {
		if call.Op == "PRSetLabels" {
			writes = append(writes, call.Vars["mode"].(string)+" "+strings.Join(anyStringsOf(call.Vars["labelIds"]), ","))
		}
	}
	if want := []string{"REMOVE " + bug.ID, "APPEND " + ui.ID}; !slices.Equal(writes, want) {
		t.Fatalf("writes = %q, want %q", writes, want)
	}

	old, oldCalls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op == "PRManageSchema" {
			return forgeAPIAnswer{Body: gitlabSchemaBare}
		}
		return forgeUnexpected(t, call)
	})
	for _, err := range []error{
		old.SetPRLabels(t.Context(), gitlabTestRef(7), PRLabelChange{Add: []PRLabel{ui}}),
		old.SetPRPeople(t.Context(), gitlabTestRef(7), true, PRPeopleChange{Add: []PRPerson{{Login: "bob"}}}),
		old.SetPRPeople(t.Context(), gitlabTestRef(7), false, PRPeopleChange{Add: []PRPerson{{Login: "bob"}}}),
	} {
		if explained, ok := errors.AsType[*ForgeActionError](err); !ok || explained.Kind != ForgeErrorRefused || !strings.Contains(explained.Message, "change them on GitLab") {
			t.Fatalf("an older host = %v", err)
		}
	}
	if ops := oldCalls.ops(); !slices.Equal(ops, []string{"PRManageSchema"}) {
		t.Fatalf("requests on an older host = %q, want one probe", ops)
	}
}

func anyStringsOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}
