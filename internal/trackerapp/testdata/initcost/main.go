// Command initcost links trackerapp and every tracker package and nothing
// else of this module beyond what they import, so GODEBUG=inittrace=1
// reports each package's own init cost, free of test variables
// (TestInitCost). It reaches each package's entry points behind a
// condition the linker cannot decide, as the app does: with no use of them
// the linker drops the package variables only they use, and an initializer
// among those would not show. A method looked up by a name known only at
// run time keeps every exported method of the values it is given linked.
package main

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jiraprovider"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linearprovider"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/trackerapi"
	"agent-overflow/internal/trackerapp"
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	arg := os.Args[1]
	reachTracker(arg)
	reachADF(arg)
	reachTrackerAPI(arg)
	jc := reachJira(arg)
	defer jc.Close()
	tc := reachTempo(arg, jc)
	defer tc.Close()
	reachJiraProvider(arg, jc, tc)
	lc := reachLinear(arg)
	defer lc.Close()
	reachLinearProvider(arg, lc)
	reachTrackerApp(arg)
}

// keepMethods exits unless every value has an exported method named arg.
func keepMethods(arg string, values ...any) {
	for _, v := range values {
		if !reflect.ValueOf(v).MethodByName(arg).IsValid() {
			os.Exit(1)
		}
	}
}

func check(err error) {
	if err != nil {
		os.Exit(1)
	}
}

func reachTracker(arg string) {
	day, _ := tracker.ParseDate(arg)
	_, _ = tracker.NewDate(len(arg), time.January, 1)
	_, _ = tracker.DateOf(time.Now())
	key, _ := tracker.FloatOrderKey(float64(len(arg)))
	_, _ = tracker.DeltaResume(time.Now(), time.Now())
	err := errors.New(arg)
	result := tracker.Refuse(tracker.StepEdit, tracker.Refusal{Err: err}, tracker.IssueID(arg))
	keepMethods(arg,
		day, key, result, tracker.Invalid(tracker.StepCreate, err), tracker.Applied(), tracker.Unknown(err), tracker.NotSent(err),
		tracker.TextValue(arg), tracker.RichTextValue(arg), tracker.NumberValue(1), tracker.DateValue(day),
		tracker.DateTimeValue(time.Now()), tracker.UserValue(), tracker.OptionValue(), tracker.ReadOnlyValue(arg),
		tracker.ClearField(), tracker.PickValues(arg), tracker.TypedValues(arg), tracker.MarkdownEdit(arg, arg),
		tracker.OnIssue(tracker.IssueID(arg)), tracker.OnCreate(tracker.ProjectID(arg), tracker.IssueTypeID(arg)),
		tracker.CardOf(tracker.IssueID(arg)), tracker.RowOf(tracker.BoardID(arg), tracker.IssueID(arg)),
		tracker.DetailOf(tracker.IssueID(arg)), tracker.StructureOf(tracker.BoardID(arg)),
		tracker.WorklogsOf(tracker.UserID(arg), day), tracker.KindText, tracker.SearchQuery{Text: arg},
		tracker.EditInput{}, tracker.CreateInput{}, tracker.MoveInput{}, tracker.RankInput{},
	)
	check(tracker.CheckLookupKeys(os.Args[1:]))
	check(result.Check())
}

func reachADF(arg string) {
	ctx := context.Background()
	doc, ws, err := adf.FromMarkdown(ctx, arg, nil)
	if err != nil || adf.CheckLoss(ws, false) != nil || adf.Validate(doc) != nil {
		os.Exit(1)
	}
	edited, _, err := adf.Edit(ctx, doc, adf.ToMarkdown(doc), nil)
	if err != nil || !adf.Equal(doc, edited) || adf.Blank(edited) {
		os.Exit(1)
	}
	adf.ReplaceMediaIDs(edited, nil)
	_ = adf.PlainText(adf.Doc(edited.Content...))
}

func reachTrackerAPI(arg string) {
	opts := trackerapi.Options{
		Name:        "probe",
		BaseURL:     arg,
		Version:     "0",
		Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte(arg)))),
		Dialect:     trackerapi.PlainDialect{},
	}
	c, err := trackerapi.New(opts)
	check(err)
	defer c.Close()
	peer, err := trackerapi.NewSharing(c, opts)
	check(err)
	defer peer.Close()
	ctx := trackerapi.WithInteractive(context.Background())
	var out any
	resp, err := c.JSON(ctx, trackerapi.Request{Path: "a"}, &out)
	if trackerapi.WriteNotApplied(err) {
		os.Exit(1)
	}
	_ = resp.Scrub(arg)
	_, _ = c.Do(ctx, trackerapi.Request{Path: "a"})
	_, _ = c.Stream(ctx, trackerapi.Request{Path: "a", Transfer: true}, io.Discard, 1)
	_, _ = c.Location(ctx, trackerapi.Request{Path: "a"})
	c.ForgetCredential()
}

func reachJira(arg string) *jira.Client {
	c, err := jira.New(jira.Options{
		BaseURL: arg, Version: "0",
		Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte(arg)))),
	})
	check(err)
	keepMethods(arg, c, jira.NewCatalog(nil))
	_, _ = jira.ParseSprintIDs(arg)
	_, _ = jira.ParseDate(arg)
	_ = jira.FieldChanges(nil, arg)
	_ = jira.IsIssueKey(arg)
	_ = jira.QuoteJQL(arg) + jira.QuoteJQLText(arg) + jira.QuoteJQLList(os.Args[1:]...)
	_, _ = jira.Text(arg), jira.RawJSON(nil)
	return c
}

func reachTempo(arg string, jc *jira.Client) *tempo.Client {
	c, err := tempo.New(tempo.Options{
		BaseURL: arg, Version: "0",
		Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte(arg)))),
		Issues:      jiraprovider.TempoIssues(jc),
	})
	check(err)
	keepMethods(arg, c)
	_, _ = tempo.ParseDate(arg)
	_, _ = tempo.ParseTimeOfDay(arg)
	_, _ = tempo.Clock(len(arg), 0, 0)
	_, _ = tempo.NewDate(len(arg), time.January, 1)
	_ = tempo.DateOf(time.Now())
	_, _, _ = tempo.CreateOp(tempo.Draft{}), tempo.UpdateOp(tempo.Change{}), tempo.DeleteOp(int64(len(arg)))
	return c
}

func reachJiraProvider(arg string, jc *jira.Client, tc *tempo.Client) {
	p, err := jiraprovider.New(jiraprovider.Options{Jira: jc, Tempo: tc, Site: arg, SprintsFrom: time.Now})
	check(err)
	keepMethods(arg, p)
}

func reachLinear(arg string) *linear.Client {
	c, err := linear.New(linear.Options{
		BaseURL: arg, Version: "0", Workspace: arg,
		Credentials: trackerapi.StaticCredentials(trackerapi.Key(secret.New([]byte(arg)))),
	})
	check(err)
	keepMethods(arg, c)
	_, _, _ = linear.ParseIdentifier(arg)
	_, _ = linear.Spread(nil, nil, len(arg))
	_, _ = linear.Between(nil, nil)
	_ = linear.IsIssueID(arg) || linear.WriteNotApplied(nil)
	_ = linear.Restart(time.Now())
	return c
}

func reachLinearProvider(arg string, c *linear.Client) {
	p, err := linearprovider.New(c)
	check(err)
	keepMethods(arg, p)
}

func reachTrackerApp(arg string) {
	svc, err := trackerapp.New(trackerapp.Deps{
		Changed:         func() {},
		CredentialsDir:  arg,
		ConfigDir:       arg,
		SetupChanged:    func() {},
		Origin:          trackerapp.APIOrigin,
		Version:         arg,
		Now:             time.Now,
		Life:            context.Background(),
		EmitBoard:       func(trackerapp.BoardEvent) {},
		EmitIssue:       func(trackerapp.IssueEvent) {},
		Settled:         func(context.Context) error { return nil },
		ClientConnected: func() bool { return false },
		LoopFailed:      func(error) {},
	})
	check(err)
	defer svc.Stop()
	keepMethods(arg, svc)
}
