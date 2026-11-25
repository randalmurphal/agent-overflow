package trackerapp

import (
	"errors"
	"testing"

	"agent-overflow/internal/tracker"
)

// TestFieldScopeInput proves a pick list's scope is an issue, an issue and
// one of its moves, or a project with its issue type, and that any other
// mix is refused before a provider is asked.
func TestFieldScopeInput(t *testing.T) {
	for _, tc := range []struct {
		in   FieldScopeInput
		want tracker.FieldScope
	}{
		{in: FieldScopeInput{Issue: "10001"}, want: tracker.OnIssue("10001")},
		{in: FieldScopeInput{Issue: "10001", Move: "31"}, want: tracker.OnMove("10001", "31")},
		{in: FieldScopeInput{Project: "10000", Type: "10002"}, want: tracker.OnCreate("10000", "10002")},
	} {
		got, err := tc.in.model()
		if err != nil || got != tc.want {
			t.Errorf("%+v: %+v %v", tc.in, got, err)
		}
	}
	for _, in := range []FieldScopeInput{{}, {Move: "31"}, {Issue: "10001", Move: "31", Project: "10000"}, {Project: "10000", Move: "31"}, {Issue: "10001", Type: "10002"}} {
		if _, err := in.model(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%+v: %v", in, err)
		}
	}
}
