package tracker

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestOutcomeIsOneState(t *testing.T) {
	cause := errors.New("cause")
	r := Refusal{Reason: ReasonTracker, Message: "no", Err: cause}
	cases := []struct {
		o       Outcome
		state   State
		refused bool
		err     error
	}{
		{Outcome{}, StateUnknown, false, nil},
		{Applied(), StateApplied, false, nil},
		{Refused(r), StateRefused, true, cause},
		{Unknown(cause), StateUnknown, false, cause},
		{NotSent(cause), StateNotSent, false, cause},
	}
	for _, c := range cases {
		got, ok := c.o.Refusal()
		if c.o.State() != c.state || ok != c.refused || c.o.Err() != c.err {
			t.Fatalf("%v: state %v, refusal %v, err %v", c.state, c.o.State(), ok, c.o.Err())
		}
		if ok && !reflect.DeepEqual(got, r) {
			t.Fatalf("refusal = %#v", got)
		}
		if !ok && !reflect.DeepEqual(got, Refusal{}) {
			t.Fatalf("%v carries a refusal %#v", c.state, got)
		}
	}
}

func part(issue IssueID, step Step, o Outcome, changed ...Resource) Part {
	return Part{Issue: issue, Step: step, Outcome: o, Changed: changed}
}

func TestResultTallyAndAllApplied(t *testing.T) {
	refused := Refused(Refusal{Reason: ReasonTracker})
	r := Result{Parts: []Part{
		part("1", StepRank, Applied(), RowOf("b", "1")),
		part("2", StepRank, refused),
		part("3", StepRank, Unknown(nil), RowOf("b", "3")),
		part("4", StepRank, NotSent(nil)),
		part("5", StepRank, Outcome{}, RowOf("b", "5")),
	}}
	if got, want := r.Tally(), (Tally{Applied: 1, Refused: 1, Unknown: 2, NotSent: 1}); got != want {
		t.Fatalf("Tally = %+v, want %+v", got, want)
	}
	if r.AllApplied() {
		t.Fatal("a mix reads as all applied")
	}
	all := Result{Parts: []Part{part("1", StepRank, Applied(), RowOf("b", "1"))}}
	if !all.AllApplied() || (Result{}).AllApplied() {
		t.Fatal("AllApplied wrong for all applied or for no parts")
	}
}

func TestResultRefreshNamesAppliedAndUnknownOnceInOrder(t *testing.T) {
	r := Result{Parts: []Part{
		part("1", StepCreate, Applied(), CardOf("1")),
		part("1", StepUpload, Unknown(nil), DetailOf("1"), CardOf("1")),
		part("1", StepEdit, NotSent(nil), DetailOf("2")),
		part("1", StepEdit, Refused(Refusal{Reason: ReasonLoss}), StructureOf("b")),
		part("", StepSprint, Outcome{}, StructureOf("b"), WorklogsOf("acc-user1", mustDate(t, "2026-11-16"))),
	}}
	want := []Resource{CardOf("1"), DetailOf("1"), StructureOf("b"), WorklogsOf("acc-user1", mustDate(t, "2026-11-16"))}
	if got := r.Refresh(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Refresh = %v, want %v", got, want)
	}
}

func mustDate(t *testing.T, s string) Date {
	t.Helper()
	d, err := ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestResultCreatedAndCreateUnknown(t *testing.T) {
	made := Result{Parts: []Part{
		part("10001", StepCreate, Applied(), CardOf("10001")),
		part("10001", StepUpload, Unknown(nil), DetailOf("10001")),
	}}
	if id, ok := made.Created(); !ok || id != "10001" || made.CreateUnknown() {
		t.Fatalf("Created = %q, %v; CreateUnknown %v", id, ok, made.CreateUnknown())
	}
	lost := Result{Parts: []Part{part("", StepCreate, Unknown(errors.New("timeout")))}}
	if _, ok := lost.Created(); ok || !lost.CreateUnknown() {
		t.Fatal("a lost create reads as made or as not lost")
	}
	if err := lost.Check(); err != nil {
		t.Fatalf("a lost create fails Check: %v", err)
	}
	refused := Refuse(StepCreate, Refusal{Reason: ReasonMissing})
	if _, ok := refused.Created(); ok || refused.CreateUnknown() {
		t.Fatal("a refused create reads as made or lost")
	}
}

func TestResultCheck(t *testing.T) {
	good := Result{Parts: []Part{
		part("10001", StepCreate, Applied(), CardOf("10001")),
		part("10001", StepUpload, NotSent(nil)),
		part("", StepSprint, Refused(Refusal{Reason: ReasonUnsupported})),
	}}
	if err := good.Check(); err != nil {
		t.Fatalf("a good result fails Check: %v", err)
	}
	bad := map[string]Result{
		"no parts":                {},
		"no step":                 {Parts: []Part{part("1", 0, NotSent(nil))}},
		"refused with no reason":  {Parts: []Part{part("1", StepEdit, Refused(Refusal{}))}},
		"applied changed nothing": {Parts: []Part{part("1", StepEdit, Applied())}},
		"unknown changed nothing": {Parts: []Part{part("1", StepEdit, Unknown(nil))}},
		"create applied no issue": {Parts: []Part{part("", StepCreate, Applied(), StructureOf("b"))}},
		"create refused issue":    {Parts: []Part{part("1", StepCreate, Refused(Refusal{Reason: ReasonInvalid}))}},
		"create lost issue":       {Parts: []Part{part("1", StepCreate, Unknown(nil), CardOf("1"))}},
		"two creates": {Parts: []Part{
			part("1", StepCreate, Applied(), CardOf("1")), part("2", StepCreate, Applied(), CardOf("2")),
		}},
		"zero resource":        {Parts: []Part{part("1", StepEdit, Applied(), Resource{})}},
		"row without board":    {Parts: []Part{part("1", StepRank, Applied(), RowOf("", "1"))}},
		"card without issue":   {Parts: []Part{part("1", StepEdit, Applied(), CardOf(""))}},
		"structure no board":   {Parts: []Part{part("", StepSprint, Applied(), StructureOf(""))}},
		"worklogs without day": {Parts: []Part{part("1", StepWorklog, Applied(), WorklogsOf("acc-user1", Date{}))}},
	}
	for name, r := range bad {
		if err := r.Check(); err == nil {
			t.Errorf("%s: Check passed", name)
		}
	}
}

func TestRefuseAndInvalid(t *testing.T) {
	r := Refusal{Reason: ReasonUnsupported, Message: "no sprints"}
	one := Refuse(StepSprint, r)
	if len(one.Parts) != 1 || one.Parts[0].Issue != "" || one.Parts[0].Outcome.State() != StateRefused {
		t.Fatalf("Refuse with no issue = %+v", one)
	}
	err := invalidf("issue %s is named twice", "1")
	many := Invalid(StepRank, err, "1", "2")
	if len(many.Parts) != 2 || many.Parts[1].Issue != "2" || many.Tally().Refused != 2 {
		t.Fatalf("Invalid = %+v", many)
	}
	got, _ := many.Parts[0].Outcome.Refusal()
	if got.Reason != ReasonInvalid || !strings.Contains(got.Message, "named twice") || !errors.Is(many.Parts[0].Outcome.Err(), ErrInvalidInput) {
		t.Fatalf("Invalid refusal = %#v", got)
	}
	if err := many.Check(); err != nil {
		t.Fatalf("Invalid's result fails Check: %v", err)
	}
}

func TestResourceAccessors(t *testing.T) {
	d := mustDate(t, "2026-11-16")
	row := RowOf("b1", "i1")
	if row.Kind() != ResourceRow || row.Board() != "b1" || row.Issue() != "i1" {
		t.Fatalf("RowOf = %+v", row)
	}
	if a, day := WorklogsOf("acc-user1", d).Worklogs(); a != "acc-user1" || day != d {
		t.Fatalf("WorklogsOf = %v %v", a, day)
	}
	if CardOf("i1") == DetailOf("i1") || RowOf("b1", "i1") == RowOf("b2", "i1") {
		t.Fatal("different resources compare equal")
	}
}
