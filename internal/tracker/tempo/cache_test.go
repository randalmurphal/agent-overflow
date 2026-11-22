package tempo

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"agent-overflow/internal/tracker/tempo/tempotest"
)

func TestAttributesCachedAndDropped(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	ctx := context.Background()
	f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	first, err := c.Attributes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Attribute{Key: "_Category_", Name: "Category", Type: AttributeList, Required: true,
		Options: []Option{{Value: "dev", Name: "Development"}, {Value: "mtg", Name: "Meeting"}}}
	head := first[0]
	head.optionsText = "" // the problem text, checked by the validation tests
	if len(first) != 5 || !reflect.DeepEqual(head, want) || first[3].Type != AttributeAccount {
		t.Fatalf("attributes %+v", first)
	}
	// The caller's copy is its own.
	first[0].Options[0].Value = "changed"
	again, err := c.Attributes(ctx)
	if err != nil || again[0].Options[0].Value != "dev" {
		t.Fatalf("cache changed through a returned copy: %+v %v", again[0], err)
	}
	if n := len(f.Requests()); n != 1 {
		t.Fatalf("%d requests, want one cached read", n)
	}

	c.DropCache()
	f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	if _, err := c.Attributes(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Requests()); n != 2 {
		t.Fatalf("%d requests after DropCache, want 2", n)
	}
}

func TestRefusedWriteDropsCache(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	openWeek(f)
	f.On("POST", "/4/worklogs", nil, createBody(10001, "x"), 400, `{"errors":[{"message":"Work attribute 'Team' is required"}]}`)
	out := c.Apply(context.Background(), []Op{CreateOp(weekDraft(t, "WEB-1", "x"))})[0]
	if out.Status != Refused {
		t.Fatalf("%+v", out)
	}
	f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	if _, err := c.Attributes(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttributesBound(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	items := make([]string, maxAttributes+1)
	for i := range items {
		items[i] = `{"self":"x","key":"_A_","name":"A","type":"INPUT_FIELD","required":false}`
	}
	f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, pageOf(0, false, items...))
	if _, err := c.Attributes(context.Background()); err == nil {
		t.Fatal("an attribute list over the bound was cached")
	}
}

func TestCacheSharesOneRead(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	started, release := make(chan struct{}), make(chan struct{})
	e := f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	e.During = func() {
		close(started)
		<-release
	}
	var wg sync.WaitGroup
	results := make([]error, 8)
	wg.Go(func() {
		_, results[0] = c.Attributes(context.Background())
	})
	<-started
	// A caller that gives up leaves without ending the shared read.
	quit, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error)
	go func() {
		_, err := c.Attributes(quit)
		gaveUp <- err
	}()
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	for i := 1; i < len(results); i++ {
		wg.Go(func() {
			_, results[i] = c.Attributes(context.Background())
		})
	}
	close(release)
	wg.Wait()
	for i, err := range results {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if n := len(f.Requests()); n != 1 {
		t.Errorf("%d reads, want one shared", n)
	}
}

// TestConcurrentUse runs reads, checks and writes on one client at once,
// for the race detector.
func TestConcurrentUse(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	// DropCache runs among the calls, so the attributes may be read again.
	f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON).Repeat = true
	f.On("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-10", "to", "2026-11-10"), "", 200, approvalJSONFor("2026-11-09", "2026-11-15", "OPEN")).Repeat = true
	day := mustDate(t, "2026-11-10")
	f.On("GET", "/4/worklogs/user/acc-user1", q("from", "2026-11-10", "to", "2026-11-10", "offset", "0", "limit", "1000"), "", 200,
		pageOf(0, false, wl{id: 1, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 60}.json())).Repeat = true
	f.On("POST", "/4/worklogs", nil, createBody(10001, "x"), 200, wl{id: 2, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 3600}.json()).Repeat = true
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 8 {
		wg.Go(func() {
			if _, err := c.Worklogs(context.Background(), WorklogQuery{Author: "acc-user1", From: day, To: day}); err != nil {
				errs <- err
			}
		})
		wg.Go(func() {
			if _, err := c.Create(context.Background(), weekDraft(t, "WEB-1", "x")); err != nil {
				errs <- err
			}
		})
		wg.Go(func() {
			if _, err := c.Attributes(context.Background()); err != nil {
				errs <- err
			}
		})
		wg.Go(func() { c.DropCache() })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestDropDuringReadIsFenced: a read that was in flight when the cache was
// dropped is not kept; its waiter reads again and gets the fresh list.
func TestDropDuringReadIsFenced(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	started, release := make(chan struct{}), make(chan struct{})
	stale := f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	stale.During = func() {
		close(started)
		<-release
	}
	f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, pageOf(0, false,
		`{"key":"_New_","name":"New","type":"INPUT_FIELD","required":false}`))
	got := make(chan []Attribute, 1)
	go func() {
		attrs, err := c.Attributes(context.Background())
		if err != nil {
			t.Error(err)
		}
		got <- attrs
	}()
	<-started
	c.DropCache()
	close(release)
	if attrs := <-got; len(attrs) != 1 || attrs[0].Key != "_New_" {
		t.Fatalf("the read dropped while in flight was kept: %+v", attrs)
	}
	again, err := c.Attributes(context.Background())
	if err != nil || len(again) != 1 || again[0].Key != "_New_" {
		t.Fatalf("cached after the fresh read: %+v %v", again, err)
	}
}
