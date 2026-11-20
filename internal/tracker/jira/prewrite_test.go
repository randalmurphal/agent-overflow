package jira

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// TestFailedReadBeforeAWriteIsNotSent proves a write whose first step, a
// read, fails sends nothing and says so: the read's 500 would otherwise
// read as a write of unknown outcome. Every GET answers 500 and any
// other method fails the test.
func TestFailedReadBeforeAWriteIsNotSent(t *testing.T) {
	var (
		mu   sync.Mutex
		gets int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("a write was sent: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		mu.Lock()
		gets++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{BaseURL: srv.URL, Version: "test", Credentials: trackerapi.StaticCredentials(
		trackerapi.Basic("user1@example.com", secret.New([]byte(testToken))))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	ctx := t.Context()
	md := "new text"
	day := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	calls := map[string]func(context.Context) error{
		"AddComment": func(ctx context.Context) error {
			// A public comment reads nothing first; the default audience
			// reads the project.
			_, err := c.AddComment(ctx, CommentInput{Issue: "WEB-1", Markdown: md, Audience: AudienceUnset})
			return err
		},
		"EditComment": func(ctx context.Context) error {
			_, err := c.EditComment(ctx, CommentEdit{Issue: "WEB-1", ID: "900", Unchecked: true, Markdown: md})
			return err
		},
		"Transition": func(ctx context.Context) error {
			_, err := c.Transition(ctx, TransitionInput{Issue: "WEB-1", Transition: "Done"})
			return err
		},
		"LinkIssues": func(ctx context.Context) error {
			return c.LinkIssues(ctx, LinkInput{Type: "Blocks", Outward: "WEB-1", Inward: "WEB-2"})
		},
		"MoveToSprint": func(ctx context.Context) error {
			_, err := c.MoveToSprint(ctx, SprintMove{Sprint: 7, Issues: []string{"WEB-1"}})
			return err
		},
		"CompleteSprint": func(ctx context.Context) error {
			_, err := c.CompleteSprint(ctx, SprintCompletion{Sprint: 7})
			return err
		},
		"SetEstimate": func(ctx context.Context) error {
			_, _, err := c.SetEstimate(ctx, EstimateInput{Issue: "WEB-1", Board: 3, Value: Points(3)})
			return err
		},
		"UpdateWorklog": func(ctx context.Context) error {
			_, err := c.UpdateWorklog(ctx, WorklogEdit{Issue: "WEB-1", ID: "300", Unchecked: true, Markdown: &md})
			return err
		},
		"UpdateSprint": func(ctx context.Context) error {
			name := "Sprint 2"
			_, err := c.UpdateSprint(ctx, SprintUpdate{ID: 7, Name: &name})
			return err
		},
		"StartSprint": func(ctx context.Context) error {
			_, err := c.StartSprint(ctx, SprintStart{ID: 7, Start: day, End: day.Add(14 * 24 * time.Hour)})
			return err
		},
	}
	for name, call := range calls {
		mu.Lock()
		before := gets
		mu.Unlock()
		err := call(ctx)
		mu.Lock()
		read := gets > before
		mu.Unlock()
		if !read {
			t.Errorf("%s read nothing first: %v", name, err)
		}
		if err == nil || !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: a failed read before the write: %v", name, err)
		}
	}
}
