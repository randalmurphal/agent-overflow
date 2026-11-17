package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

// TestConcurrentCalls runs reads, cached lookups and drops together; the
// race detector checks the client's shared state.
func TestConcurrentCalls(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
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
		// An answer that arrives as ForgetCredential drops the credential
		// its request carried cannot be checked for it, and is refused.
		if !errors.Is(err, errCredentialChanged) {
			t.Error(err)
		}
	}
}

// TestEveryCallPassesTheGuard calls every exported Client method that
// takes a context, with zero arguments otherwise, on a client that is
// closed and on one whose context ended, and wants each to fail at the
// guard before any work: nothing reaches the site, a cache hit included.
func TestEveryCallPassesTheGuard(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	closed := newTestClient(t, f)
	for _, cl := range []*Client{c, closed} {
		cl.users.put(cl.users.epoch(), "acc-user1", User{AccountID: "acc-user1", DisplayName: "User 1", Active: true})
		cl.catalog.put(cl.catalog.epoch(), struct{}{}, []Field{{ID: "summary"}})
	}
	closed.Close()
	// Calls a cache could answer.
	for name, call := range map[string]func(*Client, context.Context) error{
		"User":     func(c *Client, ctx context.Context) error { _, err := c.User(ctx, "acc-user1"); return err },
		"FindUser": func(c *Client, ctx context.Context) error { _, err := c.FindUser(ctx, "acc-user1"); return err },
		"Fields":   func(c *Client, ctx context.Context) error { _, err := c.Fields(ctx); return err },
	} {
		if err := call(c, ended); !errors.Is(err, context.Canceled) {
			t.Errorf("%s from the cache with an ended context: %v", name, err)
		}
		if err := call(closed, t.Context()); err == nil || err != closed.closedErr {
			t.Errorf("%s from the cache after Close: %v", name, err)
		}
	}
	ctxType := reflect.TypeFor[context.Context]()
	for _, tc := range []struct {
		c    *Client
		ctx  context.Context
		want func(error) bool
	}{
		{c, ended, func(err error) bool { return errors.Is(err, context.Canceled) }},
		{closed, t.Context(), func(err error) bool { _, ok := errors.AsType[*trackerapi.NotSentError](err); return ok }},
	} {
		v := reflect.ValueOf(tc.c)
		for i := range v.NumMethod() {
			m := v.Type().Method(i)
			mt := m.Type
			if mt.NumIn() < 2 || mt.In(1) != ctxType {
				continue
			}
			args := []reflect.Value{v, reflect.ValueOf(tc.ctx)}
			for j := 2; j < mt.NumIn(); j++ {
				args = append(args, reflect.Zero(mt.In(j)))
			}
			var out []reflect.Value
			if mt.IsVariadic() {
				out = m.Func.CallSlice(args)
			} else {
				out = m.Func.Call(args)
			}
			err, _ := out[len(out)-1].Interface().(error)
			if !tc.want(err) {
				t.Errorf("%s: %v", m.Name, err)
			}
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests reached the site: %v", n, f.requests())
	}
}

func TestCancelEndsACall(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
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
	c := newTestClient(t, f)
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
