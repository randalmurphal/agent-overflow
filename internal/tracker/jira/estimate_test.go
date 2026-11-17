package jira

import (
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

const estimateURL = "/rest/agile/1.0/issue/WEB-1/estimation"

// estimateSite is boards 3 (story points) and 4 (original time).
func estimateSite(t *testing.T) (*fakeJira, *Client) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", boardConfigURL, reply(200, boardConfigBody))
	f.on("GET", "/rest/agile/1.0/board/4/configuration", reply(200, `{"id":4,"type":"scrum","estimation":{"type":"field","field":{"fieldId":"timeoriginalestimate","displayName":"Original estimate"}}}`))
	f.on("GET", "/rest/agile/1.0/board/5/configuration", reply(200, `{"id":5,"type":"scrum","estimation":{"type":"issueCount"}}`))
	return f, c
}

func TestSetEstimate(t *testing.T) {
	f, c := estimateSite(t)
	var bodies []string
	f.on("PUT", estimateURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		bodies = append(bodies, r.URL.Query().Get("boardId")+" "+string(b))
		reply(200, `{"fieldId":"customfield_10016","value":"5.5"}`)(w, r, b)
	})
	field, stored, err := c.SetEstimate(t.Context(), EstimateInput{Issue: "WEB-1", Board: 3, Value: Points(5.5)})
	if err != nil || field != "customfield_10016" || stored != "5.5" {
		t.Fatalf("got %q %q %v", field, stored, err)
	}
	for _, in := range []EstimateInput{
		{Issue: "WEB-1", Board: 4, Value: TimeEstimate(90 * time.Minute)},
		{Issue: "WEB-1", Board: 3, Value: NoEstimate()},
	} {
		if _, _, err := c.SetEstimate(t.Context(), in); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{`3 {"value":"5.5"}`, `4 {"value":"90m"}`, `3 {"value":null}`}
	if strings.Join(bodies, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent\n%s", strings.Join(bodies, "\n"))
	}
	for name, in := range map[string]EstimateInput{
		"points by time":   {Issue: "WEB-1", Board: 4, Value: Points(3)},
		"time in points":   {Issue: "WEB-1", Board: 3, Value: TimeEstimate(time.Hour)},
		"no value":         {Issue: "WEB-1", Board: 3},
		"NaN":              {Issue: "WEB-1", Board: 3, Value: Points(math.NaN())},
		"negative":         {Issue: "WEB-1", Board: 3, Value: Points(-1)},
		"part of a minute": {Issue: "WEB-1", Board: 4, Value: TimeEstimate(90 * time.Second)},
		"no field":         {Issue: "WEB-1", Board: 5, Value: Points(1)},
		"no board":         {Issue: "WEB-1", Value: Points(1)},
		"a bad issue":      {Issue: "x/y", Board: 3, Value: Points(1)},
	} {
		if _, _, err := c.SetEstimate(t.Context(), in); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := f.count("PUT", estimateURL); n != 3 {
		t.Errorf("%d writes", n)
	}
}

// TestSetEstimateAnswers proves the estimation answer is read strictly:
// null is no estimate, one naming the field but otherwise malformed is
// applied with the field, and anything else is an unknown outcome that
// reports nothing stored.
func TestSetEstimateAnswers(t *testing.T) {
	for _, tc := range []struct {
		status        int
		body          string
		field, stored string
		applied, err  bool
	}{
		{200, `{"fieldId":"customfield_10016","value":null}`, "customfield_10016", "", false, false},
		{200, `{"fieldId":"customfield_10016","value":"2.0"}`, "customfield_10016", "2.0", false, false},
		{200, `{"fieldId":"customfield_10016","value":{"x":1}}`, "customfield_10016", "", true, true},
		{200, `{"fieldId":"customfield_10016","value":2}`, "customfield_10016", "", true, true},
		{200, `{"value":{"unexpected":"object"}}`, "", "", false, true},
		{200, `{"fieldId":5,"value":"2"}`, "", "", false, true},
		{200, `{"fieldId":5`, "", "", false, true},
		{200, `<html>ok</html>`, "", "", false, true},
		{204, ``, "", "", false, true},
		{201, `{"fieldId":"customfield_10016","value":"2"}`, "", "", false, true},
	} {
		f, c := estimateSite(t)
		f.on("PUT", estimateURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
			wantQuery(t, r, "boardId", "3")
			wantJSON(t, b, `{"value":"2"}`)
			reply(tc.status, tc.body)(w, r, b)
		})
		field, stored, err := c.SetEstimate(t.Context(), EstimateInput{Issue: "WEB-1", Board: 3, Value: Points(2)})
		if field != tc.field || stored != tc.stored || (err != nil) != tc.err {
			t.Errorf("%s: got %q %q %v", tc.body, field, stored, err)
			continue
		}
		if err == nil {
			continue
		}
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.Step != StepEstimate || se.Applied() != tc.applied || se.OutcomeUnknown() == tc.applied {
			t.Errorf("%s: %v", tc.body, err)
		}
	}
}
