package git

import (
	"errors"
	"fmt"
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
			if partial != tc.partial || explained == nil || !strings.HasPrefix(explained.Message, tc.want) || (explained.Kind == ForgeErrorUnconfirmed) == tc.refused {
				t.Fatalf("err = %v, explained %+v; want partial %v, refused %v, %q", err, explained, tc.partial, tc.refused, tc.want)
			}
			if !tc.refused && strings.Contains(strings.ToLower(explained.Message), "try again") {
				t.Fatalf("an unconfirmed write advises a retry: %q", explained.Message)
			}
		})
	}
}

// A batch is read against the list GitLab answered: some added and some
// missing from a whole list is a partial change naming both, also as the
// first write; missing from a list with a next page may have applied.
func TestGitLabSetBatchOutcomes(t *testing.T) {
	t.Parallel()
	page := func(more bool, usernames ...string) forgeAPIAnswer {
		nodes := make([]string, 0, len(usernames))
		for _, name := range usernames {
			nodes = append(nodes, `{"username":"`+name+`"}`)
		}
		return forgeAPIAnswer{Body: fmt.Sprintf(`{"data":{"mergeRequestSetReviewers":{"errors":[],"mergeRequest":{"reviewers":{"pageInfo":{"hasNextPage":%v},"nodes":[%s]}}}}}`, more, strings.Join(nodes, ","))}
	}
	for _, tc := range []struct {
		name          string
		change        PRPeopleChange
		remove, add   forgeAPIAnswer
		want          string
		partial, sure bool
	}{
		{name: "some added, the rest refused", change: PRPeopleChange{Add: []PRPerson{{Login: "bob"}, {Login: "eve"}}}, add: page(false, "bob"),
			want: "Added bob to the reviewers of grp/sub/app!7; adding eve was refused: GitLab did not add eve to the reviewers", partial: true, sure: true},
		{name: "some added, the rest past the page", change: PRPeopleChange{Add: []PRPerson{{Login: "bob"}, {Login: "eve"}}}, add: page(true, "bob"),
			want: "Added bob to the reviewers of grp/sub/app!7; adding eve may have applied.", partial: true},
		{name: "none shown, more pages", change: PRPeopleChange{Add: []PRPerson{{Login: "eve"}}}, add: page(true, "bob"),
			want: "GitLab did not confirm the request to change the reviewers of grp/sub/app!7"},
		{name: "a removal past the page", change: PRPeopleChange{Remove: []PRPerson{{Login: "eve"}}}, remove: page(true, "bob"),
			want: "GitLab did not confirm the request to change the reviewers of grp/sub/app!7"},
		{name: "after a removal, a batch in part", change: PRPeopleChange{Remove: []PRPerson{{Login: "carol"}}, Add: []PRPerson{{Login: "bob"}, {Login: "eve"}}},
			remove: page(false), add: page(false, "bob"),
			want: "Removed carol from the reviewers; added bob to the reviewers of grp/sub/app!7; adding eve was refused: ", partial: true, sure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				switch {
				case call.Op == "PRManageSchema":
					return forgeAPIAnswer{Body: gitlabSchemaAll}
				case call.Op == "PRSetReviewers" && call.Vars["mode"] == "REMOVE":
					return tc.remove
				case call.Op == "PRSetReviewers":
					return tc.add
				}
				return forgeUnexpected(t, call)
			})
			ref := gitlabTestRef(7)
			err := core.SetPRPeople(t.Context(), ref, true, tc.change)
			_, partial := errors.AsType[*PartialWriteError](err)
			explained, _ := errors.AsType[*ForgeActionError](ExplainForgeError(ref, ActionReviewers, err))
			if partial != tc.partial || explained == nil || !strings.HasPrefix(explained.Message, tc.want) || (explained.Kind != ForgeErrorUnconfirmed) != tc.sure {
				t.Fatalf("err = %v, explained %+v; want partial %v, sure %v, %q", err, explained, tc.partial, tc.sure, tc.want)
			}
		})
	}
}

// A label addition missing from a list of labels with a next page may
// have applied; missing from a whole list it was refused.
func TestGitLabLabelAdditionPastThePage(t *testing.T) {
	t.Parallel()
	for _, more := range []bool{true, false} {
		core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			switch call.Op {
			case "PRManageSchema":
				return forgeAPIAnswer{Body: gitlabSchemaAll}
			case "PRSetLabels":
				return forgeAPIAnswer{Body: fmt.Sprintf(`{"data":{"mergeRequestSetLabels":{"errors":[],"mergeRequest":{"labels":{"pageInfo":{"hasNextPage":%v},"nodes":[{"id":"gid://gitlab/ProjectLabel/9"}]}}}}}`, more)}
			}
			return forgeUnexpected(t, call)
		})
		err := core.SetPRLabels(t.Context(), gitlabTestRef(7), PRLabelChange{Add: []PRLabel{{ID: "gid://gitlab/ProjectLabel/1", Name: "bug"}}})
		_, unsure := errors.AsType[*UnconfirmedWriteError](err)
		_, refused := errors.AsType[*ForgeActionError](err)
		if unsure != more || refused == more {
			t.Fatalf("more %v: err = %v", more, err)
		}
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
