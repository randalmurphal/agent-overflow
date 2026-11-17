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
		reply(200, `{"fieldId":"customfield_10016","value":5.5}`)(w, r, b)
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

// TestSetEstimateUnreadable proves an estimate stored whose answer does
// not decode still names the field written, as applied.
func TestSetEstimateUnreadable(t *testing.T) {
	f, c := estimateSite(t)
	f.on("PUT", estimateURL, reply(200, `{"fieldId":5`))
	field, _, err := c.SetEstimate(t.Context(), EstimateInput{Issue: "WEB-1", Board: 3, Value: Points(2)})
	se, ok := errors.AsType[*StepError](err)
	if !ok || se.Step != StepEstimate || !se.Applied() || field != "customfield_10016" {
		t.Fatalf("got %q %v", field, err)
	}
}
