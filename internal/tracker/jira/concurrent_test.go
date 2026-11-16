package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// TestConcurrentCalls runs reads, cached lookups and drops together; the
// race detector checks the client's shared state.
func TestConcurrentCalls(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1", reply(200, fullIssueJSON))
	f.on("GET", "/rest/api/3/issue/WEB-404", reply(404, `{}`))
	f.on("GET", myselfURL, reply(200, userOneJSON))
	f.on("GET", "/rest/api/3/field", reply(200, catalogJSON))
	f.on("GET", "/rest/api/3/user", func(w http.ResponseWriter, r *http.Request, b []byte) {
		id := r.URL.Query().Get("accountId")
		reply(200, fmt.Sprintf(`{"accountId":%q,"displayName":"User","active":true}`, id))(w, r, b)
	})
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := range 40 {
		wg.Go(func() {
			if _, err := c.GetIssue(t.Context(), "WEB-1"); err != nil {
				errs <- err
			}
			if _, err := c.GetIssue(t.Context(), "WEB-404"); err == nil {
				errs <- errors.New("a 404 read as found")
			}
			if _, err := c.Fields(t.Context()); err != nil {
				errs <- err
			}
			if _, err := c.User(t.Context(), fmt.Sprintf("acc-user%d", i%5)); err != nil {
				errs <- err
			}
			switch i % 10 {
			case 0:
				c.ForgetFields()
			case 1:
				c.ForgetUsers()
			case 2:
				c.ForgetCredential()
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestCancelEndsACall(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	entered := make(chan struct{})
	f.on("POST", "/rest/api/3/search/jql", func(w http.ResponseWriter, r *http.Request, _ []byte) {
		close(entered)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() {
		_, err := c.Search(ctx, SearchQuery{JQL: "project = WEB"})
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestCloseEndsCalls(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	entered := make(chan struct{})
	f.on("GET", "/rest/api/3/issue/WEB-1", func(w http.ResponseWriter, r *http.Request, _ []byte) {
		close(entered)
		<-r.Context().Done()
	})
	done := make(chan error)
	go func() {
		_, err := c.GetIssue(context.Background(), "WEB-1")
		done <- err
	}()
	<-entered
	c.Close()
	if err := <-done; err == nil {
		t.Fatal("a call outlived Close")
	}
	if _, err := c.Myself(t.Context()); err == nil {
		t.Error("a call after Close was sent")
	}
}
