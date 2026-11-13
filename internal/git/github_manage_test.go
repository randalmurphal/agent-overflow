package git

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }

// Each management request is checked before it reaches a forge.
func TestPRManageValidation(t *testing.T) {
	t.Parallel()
	gitlabBob := PRPerson{Login: "bob", ID: "11"}
	cases := []struct {
		name string
		err  error
		ok   bool
	}{
		{"title alone", PREdit{Title: strPtr("T")}.Validate(), true},
		{"nothing", PREdit{}.Validate(), false},
		{"empty title", PREdit{Title: strPtr(" ")}.Validate(), false},
		{"two-line title", PREdit{Title: strPtr("a\nb")}.Validate(), false},
		{"description without its expected", PREdit{Description: strPtr("d")}.Validate(), false},
		{"expected without a description", PREdit{Title: strPtr("T"), ExpectedDescription: strPtr("")}.Validate(), false},
		{"description with its expected", PREdit{Description: strPtr("d"), ExpectedDescription: strPtr("")}.Validate(), true},

		{"github reviewer", PRPeopleChange{Add: []PRPerson{{Login: "bob"}}}.Validate("github", true), true},
		{"github team reviewer", PRPeopleChange{Add: []PRPerson{{Login: "acme/core", Team: true}}}.Validate("github", true), true},
		{"github team assignee", PRPeopleChange{Add: []PRPerson{{Login: "acme/core", Team: true}}}.Validate("github", false), false},
		{"gitlab team", PRPeopleChange{Add: []PRPerson{{Login: "acme/core", ID: "1", Team: true}}}.Validate("gitlab", true), false},
		{"gitlab user by login", PRPeopleChange{Add: []PRPerson{{Login: "bob"}}}.Validate("gitlab", true), true},
		{"gitlab user", PRPeopleChange{Remove: []PRPerson{gitlabBob}}.Validate("gitlab", false), true},
		{"no one", PRPeopleChange{}.Validate("github", true), false},
		{"added and removed", PRPeopleChange{Add: []PRPerson{gitlabBob}, Remove: []PRPerson{gitlabBob}}.Validate("gitlab", true), false},
		{"login with a comma", PRPeopleChange{Add: []PRPerson{{Login: "a,b"}}}.Validate("github", true), false},
		{"too many", PRPeopleChange{Add: make([]PRPerson, prPeopleMax+1)}.Validate("github", true), false},
		{"gitlab re-request without an id", validPerson("gitlab", PRPerson{Login: "bob"}, true, true), false},
		{"gitlab re-request with a gid", validPerson("gitlab", PRPerson{Login: "bob", ID: "gid://gitlab/User/11"}, true, true), false},
		{"gitlab re-request", validPerson("gitlab", gitlabBob, true, true), true},

		{"label by id", PRLabelChange{Add: []PRLabel{{ID: "LA_1"}}}.Validate(), true},
		{"label by name alone", PRLabelChange{Add: []PRLabel{{Name: "bug"}}}.Validate(), false},
		{"gitlab label with a comma", PRLabelChange{Add: []PRLabel{{ID: "gid://gitlab/ProjectLabel/1", Name: "a,b"}}}.Validate(), true},
		{"label twice", PRLabelChange{Add: []PRLabel{{ID: "L"}}, Remove: []PRLabel{{ID: "L"}}}.Validate(), false},
		{"no label", PRLabelChange{}.Validate(), false},

		{"state close", ValidatePRStateChange(PRStateClose), true},
		{"state merged", ValidatePRStateChange("merged"), false},
		{"search", ValidateCandidateSearch(CandidateLabels, "bu"), true},
		{"search kind", ValidateCandidateSearch("teams", ""), false},
		{"search two lines", ValidateCandidateSearch(CandidateReviewers, "a\nb"), false},
		{"search too long", ValidateCandidateSearch(CandidateReviewers, strings.Repeat("a", candidateQueryMax+1)), false},

		{"message with a squash", MergeRequest{Method: MergeMethodSquash, HeadSHA: strings.Repeat("a", 40), CommitMessage: &MergeMessage{Title: "T", Body: "b"}}.Validate(), true},
		{"message with a rebase", MergeRequest{Method: MergeMethodRebase, HeadSHA: strings.Repeat("a", 40), CommitMessage: &MergeMessage{Title: "T"}}.Validate(), false},
		{"message without a title", MergeRequest{Method: MergeMethodMerge, HeadSHA: strings.Repeat("a", 40), CommitMessage: &MergeMessage{Body: "b"}}.Validate(), false},
		{"message with a two-line title", MergeRequest{Method: MergeMethodMerge, HeadSHA: strings.Repeat("a", 40), CommitMessage: &MergeMessage{Title: "a\nb"}}.Validate(), false},
	}
	for _, tc := range cases {
		if (tc.err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", tc.name, tc.err, tc.ok)
		}
	}
}

// The precondition compares descriptions as the forges store them.
func TestSameDescription(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"a\r\nb", "a\nb", true},
		{"a\n", "a", true},
		{"", "\n", true},
		{"a", "a ", false},
		{"a\nb", "a\n\nb", false},
	} {
		if got := sameDescription(tc.a, tc.b); got != tc.same {
			t.Errorf("sameDescription(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
	if got := splitMergeMessage("Title\r\n\r\nBody\nmore"); got != (MergeMessage{Title: "Title", Body: "Body\nmore"}) {
		t.Errorf("splitMergeMessage = %+v", got)
	}
	if got := (MergeMessage{Title: "T"}).String(); got != "T" {
		t.Errorf("a title alone = %q", got)
	}
}

// A GitHub people change is a DELETE of the removals, then a POST of the
// additions. A failure of the first write is classified as the write's;
// one after it is a partial change, its second write refused or
// unconfirmed, each worded apart and never advising a retry when it may
// have applied.
func TestGitHubPeopleChangeWritesInOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		remove, add     int
		want            string
		partial, refuse bool
	}{
		{name: "both applied", remove: 200, add: 201},
		{name: "first refused", remove: 422, add: 201, refuse: true},
		{name: "first unanswered", remove: 502, add: 201, want: "GitHub did not confirm the request to change the reviewers of o/r#7, so it may have been applied. The pane shows what GitHub did."},
		{name: "second refused", remove: 200, add: 422, partial: true, refuse: true, want: "Removed carol from the reviewers of o/r#7; adding bob and o/core was refused: "},
		{name: "second unanswered", remove: 200, add: 502, partial: true, want: "Removed carol from the reviewers of o/r#7; adding bob and o/core may have applied. The pane shows what GitHub did."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				if call.Path != "repos/o/r/pulls/7/requested_reviewers" {
					return forgeUnexpected(t, call)
				}
				status := tc.add
				if call.Method == http.MethodDelete {
					status = tc.remove
				}
				return forgeAPIAnswer{Status: status, Body: `{"message":"Validation Failed"}`}
			})
			ref := githubTestRef(7)
			err := core.SetPRPeople(t.Context(), ref, true, PRPeopleChange{
				Add:    []PRPerson{{Login: "bob"}, {Login: "o/core", Team: true}},
				Remove: []PRPerson{{Login: "carol"}},
			})
			all := calls.all()
			if all[0].Method != http.MethodDelete || string(all[0].Body) != `{"reviewers":["carol"],"team_reviewers":[]}` {
				t.Fatalf("first write = %s %s", all[0].Method, all[0].Body)
			}
			first := tc.remove >= 300
			if first && len(all) != 1 || !first && (len(all) != 2 || all[1].Method != http.MethodPost || string(all[1].Body) != `{"reviewers":["bob"],"team_reviewers":["core"]}`) {
				t.Fatalf("writes = %+v", all)
			}
			if tc.remove < 300 && tc.add < 300 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			_, partial := errors.AsType[*PartialWriteError](err)
			_, unconfirmed := errors.AsType[*UnconfirmedWriteError](err)
			if partial != tc.partial || (!tc.partial && unconfirmed == tc.refuse) {
				t.Fatalf("err = %#v, want partial %v, refused %v", err, tc.partial, tc.refuse)
			}
			msg := ExplainForgeError(ref, ActionReviewers, err).Error()
			if tc.want != "" && !strings.HasPrefix(msg, tc.want) {
				t.Fatalf("explained = %q, want %q", msg, tc.want)
			}
			if !tc.refuse && strings.Contains(strings.ToLower(msg), "try again") {
				t.Fatalf("an unconfirmed write advises a retry: %q", msg)
			}
		})
	}
}

// A team reviewer must be a team of the PR repository's owner: a team of
// another organization with the same slug is refused before any write.
func TestGitHubTeamOfAnotherOrganizationIsRefused(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		return forgeAPIAnswer{Status: http.StatusCreated, Body: `{}`}
	})
	for _, login := range []string{"other/core", "/core", "o/"} {
		err := core.SetPRPeople(t.Context(), githubTestRef(7), true, PRPeopleChange{Add: []PRPerson{{Login: login, Team: true}}})
		if explained, ok := errors.AsType[*ForgeActionError](err); !ok || explained.Kind != ForgeErrorRefused {
			t.Fatalf("%s = %v, want a refusal", login, err)
		}
	}
	if n := len(calls.all()); n != 0 {
		t.Fatalf("writes sent = %d", n)
	}
	if err := core.SetPRPeople(t.Context(), githubTestRef(7), true, PRPeopleChange{Add: []PRPerson{{Login: "O/core", Team: true}}}); err != nil {
		t.Fatalf("the owner's team, another case: %v", err)
	}
	if got := string(calls.all()[0].Body); got != `{"reviewers":[],"team_reviewers":["core"]}` {
		t.Fatalf("body = %s", got)
	}
}

// GitHub keeps ten assignees: a change that would leave more is refused
// before any write, counting what it removes.
func TestGitHubAssigneeLimit(t *testing.T) {
	t.Parallel()
	nodes := make([]string, 0, 10)
	for i := range 10 {
		nodes = append(nodes, `{"login":"u`+string(rune('a'+i))+`"}`)
	}
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op == "PRAssignees" {
			return githubData(`{"repository":{"pullRequest":{"assignees":{"nodes":[` + strings.Join(nodes, ",") + `]}}}}`)
		}
		return forgeAPIAnswer{Status: http.StatusOK, Body: `{}`}
	})
	ref := githubTestRef(7)
	err := core.SetPRPeople(t.Context(), ref, false, PRPeopleChange{Add: []PRPerson{{Login: "eleven"}}})
	if explained, ok := errors.AsType[*ForgeActionError](err); !ok || !strings.Contains(explained.Message, "at most 10 assignees") {
		t.Fatalf("an eleventh = %v", err)
	}
	if ops := calls.ops(); len(ops) != 1 {
		t.Fatalf("requests = %q, want the assignee read alone", ops)
	}
	if err := core.SetPRPeople(t.Context(), ref, false, PRPeopleChange{Add: []PRPerson{{Login: "eleven"}}, Remove: []PRPerson{{Login: "ua"}}}); err != nil {
		t.Fatalf("a replacement at the limit: %v", err)
	}
	ops := calls.ops()
	if want := []string{"PRAssignees", "PRAssignees", "DELETE repos/o/r/issues/7/assignees", "POST repos/o/r/issues/7/assignees"}; !slices.Equal(ops, want) {
		t.Fatalf("requests = %q, want %q", ops, want)
	}
}

// An unconfirmed write and a partial one explain themselves even around
// an explained cause; explaining twice changes nothing.
func TestExplainUnconfirmedWrite(t *testing.T) {
	t.Parallel()
	ref := gitlabTestRef(2)
	cause := &ForgeActionError{Kind: ForgeErrorRefused, Message: "GitLab refused: Reviewer not found."}
	err := ExplainForgeError(ref, ActionReviewers, &PartialWriteError{Done: "removed bob from the reviewers", Next: "adding carol", Err: cause})
	explained, ok := errors.AsType[*ForgeActionError](err)
	want := "Removed bob from the reviewers of grp/sub/app!2; adding carol was refused: GitLab refused: Reviewer not found."
	if !ok || explained.Kind != ForgeErrorRefused || explained.Message != want {
		t.Fatalf("partial = %+v, want %q", explained, want)
	}
	if again := ExplainForgeError(ref, ActionReviewers, err); again != err {
		t.Fatalf("explained twice = %v", again)
	}
	unsure, _ := errors.AsType[*ForgeActionError](ExplainForgeError(ref, ActionReviewers, &PartialWriteError{Done: "removed bob from the reviewers", Next: "adding carol", Err: &UnconfirmedWriteError{Err: cause}}))
	if unsure == nil || unsure.Kind != ForgeErrorUnconfirmed || unsure.Message != "Removed bob from the reviewers of grp/sub/app!2; adding carol may have applied. The pane shows what GitLab did." {
		t.Fatalf("partial, second unconfirmed = %+v", unsure)
	}
	whole, _ := errors.AsType[*ForgeActionError](ExplainForgeError(ref, ActionClose, &UnconfirmedWriteError{Err: errors.New("EOF")}))
	if whole == nil || whole.Kind != ForgeErrorUnconfirmed || !strings.HasPrefix(whole.Message, "GitLab did not confirm the request to close grp/sub/app!2") {
		t.Fatalf("unconfirmed = %+v", whole)
	}
}
