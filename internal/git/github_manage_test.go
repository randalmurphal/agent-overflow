package git

import (
	"errors"
	"net/http"
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
		{"gitlab user without an id", PRPeopleChange{Add: []PRPerson{{Login: "bob"}}}.Validate("gitlab", true), false},
		{"gitlab user id not numeric", PRPeopleChange{Add: []PRPerson{{Login: "bob", ID: "gid://gitlab/User/11"}}}.Validate("gitlab", true), false},
		{"gitlab user", PRPeopleChange{Remove: []PRPerson{gitlabBob}}.Validate("gitlab", false), true},
		{"no one", PRPeopleChange{}.Validate("github", true), false},
		{"added and removed", PRPeopleChange{Add: []PRPerson{gitlabBob}, Remove: []PRPerson{gitlabBob}}.Validate("gitlab", true), false},
		{"login with a comma", PRPeopleChange{Add: []PRPerson{{Login: "a,b"}}}.Validate("github", true), false},
		{"too many", PRPeopleChange{Add: make([]PRPerson, prPeopleMax+1)}.Validate("github", true), false},

		{"github label by id", PRLabelChange{Add: []PRLabel{{ID: "LA_1"}}}.Validate("github"), true},
		{"github label by name alone", PRLabelChange{Add: []PRLabel{{Name: "bug"}}}.Validate("github"), false},
		{"gitlab label by name", PRLabelChange{Remove: []PRLabel{{Name: "bug"}}}.Validate("gitlab"), true},
		{"gitlab label with a comma", PRLabelChange{Add: []PRLabel{{Name: "a,b"}}}.Validate("gitlab"), false},
		{"no label", PRLabelChange{}.Validate("gitlab"), false},

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

// A GitHub people change is a POST of the additions and a DELETE of the
// removals. A failure of the first write is classified as the write's;
// one after it is a partial change, explained as such.
func TestGitHubPeopleChangeWritesInOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		post, delete  int
		partial, sure bool
	}{
		{name: "both applied", post: 201, delete: 200},
		{name: "first refused", post: 422, delete: 200, sure: true},
		{name: "first unanswered", post: 502, delete: 200},
		{name: "second refused", post: 201, delete: 422, partial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				if call.Path != "repos/o/r/pulls/7/requested_reviewers" {
					return forgeUnexpected(t, call)
				}
				status := tc.post
				if call.Method == http.MethodDelete {
					status = tc.delete
				}
				return forgeAPIAnswer{Status: status, Body: `{"message":"Validation Failed"}`}
			})
			ref := githubTestRef(7)
			err := core.SetPRPeople(t.Context(), ref, true, PRPeopleChange{
				Add:    []PRPerson{{Login: "bob"}, {Login: "o/core", Team: true}},
				Remove: []PRPerson{{Login: "carol"}},
			})
			all := calls.all()
			if all[0].Method != http.MethodPost || string(all[0].Body) != `{"reviewers":["bob"],"team_reviewers":["core"]}` {
				t.Fatalf("first write = %s %s", all[0].Method, all[0].Body)
			}
			first := tc.post >= 300
			if first && len(all) != 1 || !first && (len(all) != 2 || all[1].Method != http.MethodDelete || string(all[1].Body) != `{"reviewers":["carol"],"team_reviewers":[]}`) {
				t.Fatalf("writes = %+v", all)
			}
			unconfirmed, isUnconfirmed := errors.AsType[*UnconfirmedWriteError](err)
			switch {
			case tc.post < 300 && tc.delete < 300:
				if err != nil {
					t.Fatal(err)
				}
			case tc.sure:
				if err == nil || isUnconfirmed {
					t.Fatalf("a refused first write = %v, want the refusal", err)
				}
			default:
				if !isUnconfirmed || unconfirmed.Partial != tc.partial {
					t.Fatalf("err = %v, want unconfirmed (partial %v)", err, tc.partial)
				}
				msg := ExplainForgeError(ref, ActionReviewers, err).Error()
				if !strings.Contains(msg, "The pane shows what GitHub did.") || strings.Contains(msg, "applied part") != tc.partial {
					t.Fatalf("explained = %q", msg)
				}
			}
		})
	}
}

// An unconfirmed write explains itself even around an explained cause;
// explaining it twice changes nothing.
func TestExplainUnconfirmedWrite(t *testing.T) {
	t.Parallel()
	ref := gitlabTestRef(2)
	cause := &ForgeActionError{Kind: ForgeErrorRefused, Message: "GitLab refused: Reviewer not found."}
	err := ExplainForgeError(ref, ActionReviewers, &UnconfirmedWriteError{Err: cause, Partial: true})
	explained, ok := errors.AsType[*ForgeActionError](err)
	want := "GitLab applied part of the change to grp/sub/app!2, then: GitLab refused: Reviewer not found. The pane shows what GitLab did."
	if !ok || explained.Kind != ForgeErrorRefused || explained.Message != want {
		t.Fatalf("partial = %+v, want %q", explained, want)
	}
	if again := ExplainForgeError(ref, ActionReviewers, err); again != err {
		t.Fatalf("explained twice = %v", again)
	}
	whole, _ := errors.AsType[*ForgeActionError](ExplainForgeError(ref, ActionClose, &UnconfirmedWriteError{Err: errors.New("EOF")}))
	if whole == nil || whole.Kind != ForgeErrorTransient || !strings.HasPrefix(whole.Message, "GitLab did not confirm the request to close grp/sub/app!2") {
		t.Fatalf("unconfirmed = %+v", whole)
	}
}
