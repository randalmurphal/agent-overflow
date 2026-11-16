package tempo

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestAttributesCachedAndDropped(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	ctx := context.Background()
	f.on("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	first, err := c.Attributes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Attribute{Key: "_Category_", Name: "Category", Type: AttributeList, Required: true,
		Options: []Option{{Value: "dev", Name: "Development"}, {Value: "mtg", Name: "Meeting"}}}
	if len(first) != 5 || !reflect.DeepEqual(first[0], want) || first[3].Type != AttributeAccount {
		t.Fatalf("attributes %+v", first)
	}
	// The caller's copy is its own.
	first[0].Options[0].Value = "changed"
	again, err := c.Attributes(ctx)
	if err != nil || again[0].Options[0].Value != "dev" {
		t.Fatalf("cache changed through a returned copy: %+v %v", again[0], err)
	}
	if n := len(f.requests()); n != 1 {
		t.Fatalf("%d requests, want one cached read", n)
	}

	c.DropCache()
	f.on("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	if _, err := c.Attributes(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests()); n != 2 {
		t.Fatalf("%d requests after DropCache, want 2", n)
	}
}

func TestRefusedWriteDropsCache(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	openWeek(f)
	f.on("POST", "/4/worklogs", nil, createBody(10001, "x"), 400, `{"errors":[{"message":"Work attribute 'Team' is required"}]}`)
	out := c.Apply(context.Background(), []Op{CreateOp(weekDraft(t, "WEB-1", "x"))})[0]
	if out.Status != Refused {
		t.Fatalf("%+v", out)
	}
	f.on("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	if _, err := c.Attributes(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttributesBound(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	items := make([]string, maxAttributes+1)
	for i := range items {
		items[i] = `{"self":"x","key":"_A_","name":"A","type":"INPUT_FIELD","required":false}`
	}
	f.on("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, pageOf(0, false, items...))
	if _, err := c.Attributes(context.Background()); err == nil {
		t.Fatal("an attribute list over the bound was cached")
	}
}

func TestCacheSharesOneRead(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	started, release := make(chan struct{}), make(chan struct{})
	e := f.on("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	e.during = func() {
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
	if n := len(f.requests()); n != 1 {
		t.Errorf("%d reads, want one shared", n)
	}
}

// TestConcurrentUse runs reads, checks and writes on one client at once,
// for the race detector.
func TestConcurrentUse(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	// DropCache runs among the calls, so the attributes may be read again.
	f.on("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON).repeat = true
	f.on("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-10", "to", "2026-11-10"), "", 200, approvalJSONFor("2026-11-09", "2026-11-15", "OPEN")).repeat = true
	day := mustDate(t, "2026-11-10")
	f.on("GET", "/4/worklogs/user/acc-user1", q("from", "2026-11-10", "to", "2026-11-10", "offset", "0", "limit", "1000"), "", 200,
		pageOf(0, false, wl{id: 1, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 60}.json())).repeat = true
	f.on("POST", "/4/worklogs", nil, createBody(10001, "x"), 200, wl{id: 2, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 3600}.json()).repeat = true
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
