// Command initcost links the tracker package and nothing else of this
// module beyond what it imports, so GODEBUG=inittrace=1 reports the
// package's own init cost, free of test variables (TestInitCost). It
// reaches the package's entry points behind a condition the linker cannot
// decide, as the app does: with no use of them the linker drops the
// package variables only they use, and an initializer among those would
// not show. A method looked up by a name known only at run time keeps
// every exported method of the values below linked.
package main

import (
	"errors"
	"os"
	"reflect"
	"time"

	"agent-overflow/internal/tracker"
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	arg := os.Args[1]
	day, _ := tracker.ParseDate(arg)
	_, _ = tracker.NewDate(len(arg), time.January, 1)
	_, _ = tracker.DateOf(time.Now())
	key, _ := tracker.FloatOrderKey(float64(len(arg)))
	_, _ = tracker.DeltaResume(time.Now(), time.Now())
	err := errors.New(arg)
	result := tracker.Refuse(tracker.StepEdit, tracker.Refusal{Err: err}, tracker.IssueID(arg))
	values := []any{
		day, key, result, tracker.Invalid(tracker.StepCreate, err), tracker.Applied(), tracker.Unknown(err), tracker.NotSent(err),
		tracker.TextValue(arg), tracker.RichTextValue(arg), tracker.NumberValue(1), tracker.DateValue(day),
		tracker.DateTimeValue(time.Now()), tracker.UserValue(), tracker.OptionValue(), tracker.ReadOnlyValue(arg),
		tracker.ClearField(), tracker.PickValues(arg), tracker.TypedValues(arg), tracker.MarkdownEdit(arg, arg),
		tracker.OnIssue(tracker.IssueID(arg)), tracker.OnCreate(tracker.ProjectID(arg), tracker.IssueTypeID(arg)),
		tracker.CardOf(tracker.IssueID(arg)), tracker.RowOf(tracker.BoardID(arg), tracker.IssueID(arg)),
		tracker.DetailOf(tracker.IssueID(arg)), tracker.StructureOf(tracker.BoardID(arg)),
		tracker.WorklogsOf(tracker.UserID(arg), day), tracker.KindText, tracker.SearchQuery{Text: arg},
		tracker.EditInput{}, tracker.CreateInput{}, tracker.MoveInput{}, tracker.RankInput{},
	}
	for _, v := range values {
		if !reflect.ValueOf(v).MethodByName(arg).IsValid() {
			os.Exit(1)
		}
	}
	if tracker.CheckLookupKeys(os.Args[1:]) != nil || result.Check() != nil {
		os.Exit(1)
	}
}
