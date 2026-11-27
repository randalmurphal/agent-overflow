package tracker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// probe tells whether a context is inside a walk: a walk admits one
// read of a client's credential, so of two different marks of one
// client it refuses the second; outside a walk both join.
type probe struct{ a, b trackerapi.CredentialMark }

// newProbe reads two marks of one client: one request, a forgotten
// credential, and another request under the source's next token.
func newProbe(t *testing.T) probe {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	var n atomic.Int32
	c, err := trackerapi.New(trackerapi.Options{
		Name: "probe", BaseURL: srv.URL, Version: "test", Dialect: trackerapi.PlainDialect{},
		Credentials: sourceFunc(func() *trackerapi.Credential {
			return trackerapi.Bearer(secret.New(fmt.Appendf(nil, "probe-token-%d", n.Add(1))))
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	mark := func() trackerapi.CredentialMark {
		resp, err := c.Do(t.Context(), trackerapi.Request{Path: "x"})
		if err != nil {
			t.Fatal(err)
		}
		return resp.CredentialMark()
	}
	a := mark()
	c.ForgetCredential()
	b := mark()
	if a == b {
		t.Fatal("two reads of the credential gave one mark")
	}
	return probe{a, b}
}

type sourceFunc func() *trackerapi.Credential

func (f sourceFunc) Credential(context.Context) (*trackerapi.Credential, error) { return f(), nil }

func (p probe) inWalk(ctx context.Context) bool {
	return trackerapi.JoinWalk(ctx, p.a) == nil && trackerapi.JoinWalk(ctx, p.b) != nil
}

// probed records whether a method of probedProvider or probedTime ran
// inside a walk.
type probed struct {
	p      probe
	walked *bool
}

func (f probed) see(ctx context.Context) { *f.walked = f.p.inWalk(ctx) }

// TestOneCredentialWalksEveryRead calls every method of a OneCredential
// provider, of its time tracker and of its poker: each read runs inside a
// walk, each write outside one.
func TestOneCredentialWalksEveryRead(t *testing.T) {
	pr := newProbe(t)
	var walked bool
	inner := &probedProvider{probed{p: pr, walked: &walked}}
	wrapped := OneCredential(inner)
	check := func(v reflect.Value, typ reflect.Type) {
		for i := range typ.NumMethod() {
			m := typ.Method(i)
			ft := m.Type
			if ft.NumIn() == 0 || ft.In(0) != reflect.TypeFor[context.Context]() {
				continue
			}
			args := []reflect.Value{reflect.ValueOf(t.Context())}
			for j := 1; j < ft.NumIn(); j++ {
				args = append(args, reflect.Zero(ft.In(j)))
			}
			walked = false
			v.MethodByName(m.Name).Call(args)
			isWrite := ft.NumOut() == 1 && ft.Out(0) == reflect.TypeFor[Result]()
			if walked == isWrite {
				t.Errorf("%s ran inside a walk: %v; want %v", m.Name, walked, !isWrite)
			}
		}
	}
	check(reflect.ValueOf(wrapped), reflect.TypeFor[Provider]())
	check(reflect.ValueOf(wrapped.Time()), reflect.TypeFor[TimeTracker]())
	check(reflect.ValueOf(wrapped.Poker()), reflect.TypeFor[Poker]())
	if pr.inWalk(t.Context()) {
		t.Error("the probe finds a walk outside one")
	}
}

// probedProvider records the walk of every method.
type probedProvider struct{ probed }

func (f *probedProvider) Capabilities() Capabilities                 { return Capabilities{} }
func (f *probedProvider) Identity(ctx context.Context) (User, error) { f.see(ctx); return User{}, nil }
func (f *probedProvider) Projects(ctx context.Context) ([]Project, error) {
	f.see(ctx)
	return nil, nil
}
func (f *probedProvider) PullRequests(ctx context.Context, _ IssueID) ([]LinkedPullRequest, error) {
	f.see(ctx)
	return nil, nil
}
func (f *probedProvider) LinkTypes(ctx context.Context) ([]LinkType, error) {
	f.see(ctx)
	return nil, nil
}
func (f *probedProvider) Fields(ctx context.Context) ([]Field, error) {
	f.see(ctx)
	return nil, nil
}
func (f *probedProvider) Boards(ctx context.Context, _ ProjectID) ([]Board, error) {
	f.see(ctx)
	return nil, nil
}
func (f *probedProvider) BoardProjects(ctx context.Context, _ BoardID) ([]Project, error) {
	f.see(ctx)
	return nil, nil
}
func (f *probedProvider) IssueTypes(ctx context.Context, _ ProjectID) ([]IssueType, error) {
	f.see(ctx)
	return nil, nil
}
func (f *probedProvider) Structure(ctx context.Context, _ BoardID, _ time.Time) (Structure, error) {
	f.see(ctx)
	return Structure{}, nil
}
func (f *probedProvider) ChangedRows(ctx context.Context, _ DeltaQuery) (RowPage, error) {
	f.see(ctx)
	return RowPage{}, nil
}
func (f *probedProvider) BoardIssues(ctx context.Context, _ BoardID, _ time.Time) (Listing, error) {
	f.see(ctx)
	return Listing{}, nil
}
func (f *probedProvider) BoardRows(ctx context.Context, _ BoardID, _ []IssueID, _ Eligibility) (Rows, error) {
	f.see(ctx)
	return Rows{}, nil
}
func (f *probedProvider) NewRows(ctx context.Context, _ IssueID, _ []BoardID) (Placement, error) {
	f.see(ctx)
	return Placement{}, nil
}
func (f *probedProvider) Cards(ctx context.Context, _ []IssueID) (Cards, error) {
	f.see(ctx)
	return Cards{}, nil
}
func (f *probedProvider) History(ctx context.Context, _ HistoryQuery) (History, error) {
	f.see(ctx)
	return History{}, nil
}
func (f *probedProvider) Evidence(ctx context.Context, _ EvidenceQuery) (Evidence, error) {
	f.see(ctx)
	return Evidence{}, nil
}
func (f *probedProvider) Detail(ctx context.Context, _ IssueID) (Detail, error) {
	f.see(ctx)
	return Detail{}, nil
}
func (f *probedProvider) Details(ctx context.Context, _ []IssueID) (Details, error) {
	f.see(ctx)
	return Details{}, nil
}
func (f *probedProvider) Moves(ctx context.Context, _ IssueID) ([]Move, error) {
	f.see(ctx)
	return nil, nil
}
func (f *probedProvider) Comments(ctx context.Context, _ IssueID, _ PageToken) (CommentPage, error) {
	f.see(ctx)
	return CommentPage{}, nil
}
func (f *probedProvider) CreateFields(ctx context.Context, _ ProjectID, _ IssueTypeID) ([]ScreenField, error) {
	f.see(ctx)
	return nil, nil
}
func (f *probedProvider) AssignableUsers(ctx context.Context, _ FieldScope, _ string) (UserList, error) {
	f.see(ctx)
	return UserList{}, nil
}
func (f *probedProvider) Choices(ctx context.Context, _ FieldScope, _ FieldID, _ string) (ChoiceList, error) {
	f.see(ctx)
	return ChoiceList{}, nil
}
func (f *probedProvider) Lookup(ctx context.Context, _ []string) (KeyLookup, error) {
	f.see(ctx)
	return KeyLookup{}, nil
}
func (f *probedProvider) Search(ctx context.Context, _ SearchQuery) (SearchPage, error) {
	f.see(ctx)
	return SearchPage{}, nil
}
func (f *probedProvider) Download(ctx context.Context, _ DownloadInput) error { f.see(ctx); return nil }
func (f *probedProvider) Create(ctx context.Context, _ CreateInput) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) Edit(ctx context.Context, _ EditInput) Result { f.see(ctx); return Result{} }
func (f *probedProvider) Move(ctx context.Context, _ MoveInput) Result { f.see(ctx); return Result{} }
func (f *probedProvider) SetEstimate(ctx context.Context, _ EstimateInput) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) Rank(ctx context.Context, _ RankInput) Result { f.see(ctx); return Result{} }
func (f *probedProvider) MoveToSprint(ctx context.Context, _ SprintMoveInput) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) CreateSprint(ctx context.Context, _ SprintInput) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) StartSprint(ctx context.Context, _ SprintStart) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) CompleteSprint(ctx context.Context, _ SprintCompletion) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) EditSprint(ctx context.Context, _ SprintEdit) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) AddComment(ctx context.Context, _ CommentInput) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) EditComment(ctx context.Context, _ CommentEdit) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) DeleteComment(ctx context.Context, _ CommentDelete) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) Link(ctx context.Context, _ LinkInput) Result { f.see(ctx); return Result{} }
func (f *probedProvider) Unlink(ctx context.Context, _ UnlinkInput) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) Attach(ctx context.Context, _ AttachInput) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedProvider) Time() TimeTracker { return &probedTime{f.probed} }

type probedTime struct{ probed }

func (f *probedTime) Worklogs(ctx context.Context, _ WorklogQuery) ([]Worklog, error) {
	f.see(ctx)
	return nil, nil
}
func (f *probedTime) AddWorklogs(ctx context.Context, _ []WorklogInput) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedTime) EditWorklog(ctx context.Context, _ WorklogEdit) Result {
	f.see(ctx)
	return Result{}
}
func (f *probedTime) DeleteWorklog(ctx context.Context, _ WorklogDelete) Result {
	f.see(ctx)
	return Result{}
}

func (f *probedProvider) Poker() Poker { return &probedPoker{f.probed} }

type probedPoker struct{ probed }

func (f *probedPoker) Support(ctx context.Context) (bool, error) { f.see(ctx); return false, nil }
func (f *probedPoker) Read(ctx context.Context, _ IssueID) (PokerRound, error) {
	f.see(ctx)
	return PokerRound{}, nil
}
func (f *probedPoker) Vote(ctx context.Context, _ PokerVoteInput) Result { f.see(ctx); return Result{} }
func (f *probedPoker) Reveal(ctx context.Context, _ IssueID) Result      { f.see(ctx); return Result{} }
func (f *probedPoker) Clear(ctx context.Context, _ IssueID) Result       { f.see(ctx); return Result{} }
