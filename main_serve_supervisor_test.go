package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/supervise"
)

// supervisorPair builds a child-side channel plus the supervisor end of it, so
// a test can read what the backend sent and decide whether to answer.
func supervisorPair(t *testing.T, timeout time.Duration) (*serveSupervisor, *os.File, *os.File) {
	t.Helper()
	downR, downW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	upR, upW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() {
		downR.Close()
		downW.Close()
		upR.Close()
		upW.Close()
	})
	sup := &serveSupervisor{
		conn:          supervise.NewConn(downR, upW, nil),
		answerTimeout: timeout,
		committed:     make(chan struct{}),
	}
	return sup, upR, downW
}

// readFrame takes one JSON line off the supervisor's end.
func readFrame(t *testing.T, r *os.File) supervise.Message {
	t.Helper()
	buf := make([]byte, 4096)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("read the child's frame: %v", err)
	}
	var msg supervise.Message
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(buf[:n]))), &msg); err != nil {
		t.Fatalf("decode %q: %v", string(buf[:n]), err)
	}
	return msg
}

// The answer arrives on a goroutine that ends when the channel does, so a
// supervisor whose loop is wedged while its process lives would never end the
// wait on its own. An unbounded wait leaves the caller's one-flow fence
// claimed for the life of the process and every later request refused as busy,
// which is the state a person cannot get out of without restarting a machine
// they are not at.
func TestAnUnansweredUpdateRequestGivesUpAndLetsTheNextOneThrough(t *testing.T) {
	sup, up, _ := supervisorPair(t, 20*time.Millisecond)

	for attempt := 1; attempt <= 2; attempt++ {
		done := make(chan error, 1)
		go func() {
			_, err := sup.RequestUpdate("2.0.0")
			done <- err
		}()

		if msg := readFrame(t, up); msg.Type != supervise.MsgRequestUpdate || msg.TargetVersion != "2.0.0" {
			t.Fatalf("attempt %d sent %+v, want a request for 2.0.0", attempt, msg)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("attempt %d was answered by a supervisor that said nothing", attempt)
			}
			// The second attempt is the point: a request that gave up must
			// have released the waiter, or this reads "already waiting".
			if !strings.Contains(err.Error(), "did not answer") {
				t.Fatalf("attempt %d failed with %v, want the timeout", attempt, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d never gave up", attempt)
		}
	}
}

// And the ordinary path still works: an answer inside the budget is the id the
// client correlates its reconnect against.
func TestAnAnsweredUpdateRequestReturnsTheSupervisorsID(t *testing.T) {
	sup, up, down := supervisorPair(t, 10*time.Second)
	go sup.read()

	done := make(chan string, 1)
	fail := make(chan error, 1)
	go func() {
		id, err := sup.RequestUpdate("2.0.0")
		if err != nil {
			fail <- err
			return
		}
		done <- id
	}()

	readFrame(t, up)
	if err := supervise.NewConn(up, down, nil).Send(supervise.Message{
		Type: supervise.MsgUpdateAccepted, UpdateID: "upd-7",
	}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	select {
	case id := <-done:
		if id != "upd-7" {
			t.Fatalf("update id = %q, want the supervisor's own", id)
		}
	case err := <-fail:
		t.Fatalf("RequestUpdate: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("an answered request never returned")
	}
}
