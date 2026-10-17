package app

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// A request's work may start as the last one ends while shutdown, the
// harness or a test waits on threadRequestsWG: the emit path starts a
// drain from whatever goroutine emitted. The Wait returns at the moment
// the count reached zero, and the new member is counted for the next one.
// With one P the waiter cannot run between the Done and the Add, which is
// the order a sync.WaitGroup panics on ("WaitGroup is reused before
// previous Wait has returned"). Serial: it sets GOMAXPROCS.
func TestThreadRequestWorkMayStartAsAWaitReturns(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var a App
	g := &a.threadRequestsWG

	g.Add(1)
	waited := make(chan struct{})
	go waitThreadRequestWork(g, waited)
	awaitParkedIn(t, "waitThreadRequestWork")
	select {
	case <-waited:
		t.Fatal("Wait returned with a member running")
	default:
	}

	g.Done()
	g.Add(1)
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return at the moment the count reached zero")
	}

	again := make(chan struct{})
	go waitThreadRequestWork(g, again)
	awaitParkedIn(t, "waitThreadRequestWork")
	g.Done()
	select {
	case <-again:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return once the member that started during the last one ended")
	}
	g.Wait()
}

func waitThreadRequestWork(g interface{ Wait() }, done chan<- struct{}) {
	g.Wait()
	close(done)
}

// awaitParkedIn yields until a goroutine running fn is blocked in a Wait.
func awaitParkedIn(t *testing.T, fn string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		for _, stack := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
			header, _, _ := strings.Cut(stack, "\n")
			if strings.Contains(stack, "."+fn+"(") && strings.Contains(header, ".Wait") {
				return
			}
		}
		runtime.Gosched()
	}
	t.Fatalf("no goroutine in %s parked in a Wait", fn)
}
