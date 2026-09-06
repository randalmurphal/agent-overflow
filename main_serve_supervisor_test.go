package main

import (
	"encoding/json"
	"errors"
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
		restart:       make(chan struct{}),
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

// A timeout cannot recycle an uncorrelated reply slot. The parent may still
// accept the first request, so drain and restart before accepting more work.
func TestAnUnansweredUpdateRequestsRestartAndRejectsRetry(t *testing.T) {
	sup, up, _ := supervisorPair(t, 20*time.Millisecond)
	done := make(chan error, 1)
	go func() { _, err := sup.RequestUpdate("2.0.0"); done <- err }()
	if msg := readFrame(t, up); msg.Type != supervise.MsgRequestUpdate {
		t.Fatal(msg)
	}
	select {
	case err := <-done:
		if !errors.Is(err, supervise.ErrUpdateOutcomeUnknown) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unanswered request never settled")
	}
	select {
	case <-sup.restartRequested():
	default:
		t.Fatal("ambiguous result did not request orderly restart")
	}
	// A late acknowledgment must not be delivered to a newer request.
	sup.deliver(supervise.Message{Type: supervise.MsgUpdateAccepted, UpdateID: "late"})
	if _, err := sup.RequestUpdate("3.0.0"); !errors.Is(err, supervise.ErrUpdateOutcomeUnknown) {
		t.Fatalf("retry: %v", err)
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
