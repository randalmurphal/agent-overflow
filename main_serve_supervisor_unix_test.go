//go:build !windows

package main

import (
	"errors"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

// attachAsChild runs attachServeSupervisor over a real inherited channel
// opened with activate, and returns the child's end plus the supervisor's.
func attachAsChild(t *testing.T, activate supervise.Message) (*serveSupervisor, *supervise.Conn) {
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
	// A frame that never comes fails the test instead of hanging it.
	if err := upR.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	parent := supervise.NewConn(upR, downW, nil)
	if err := parent.Send(activate); err != nil {
		t.Fatalf("send activate: %v", err)
	}
	// The channel owns the descriptors it opens, so it gets copies.
	childRead, err := syscall.Dup(int(downR.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	childWrite, err := syscall.Dup(int(upW.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	t.Setenv(supervise.EnvChannel, strconv.Itoa(childRead)+","+strconv.Itoa(childWrite))
	sup, err := attachServeSupervisor()
	if err != nil {
		t.Fatalf("attachServeSupervisor: %v", err)
	}
	if sup == nil {
		t.Fatal("attachServeSupervisor found no supervisor")
	}
	t.Cleanup(func() { sup.conn.Close() })
	return sup, parent
}

func receive(t *testing.T, parent *supervise.Conn, want string) supervise.Message {
	t.Helper()
	msg, err := parent.Receive()
	if err != nil {
		t.Fatalf("receive %s: %v", want, err)
	}
	if msg.Type != want {
		t.Fatalf("received %+v, want a %s frame", msg, want)
	}
	return msg
}

// A serve trial says it reports progress, forwards its startup reports, and
// reports a failed start as the failure it is. Nothing is forwarded after
// Start returned.
func TestAServeTrialReportsItsProgressAndItsFailedStart(t *testing.T) {
	sup, parent := attachAsChild(t, supervise.Message{
		Type: supervise.MsgActivate, ProtocolVersion: supervise.ProtocolVersion,
		Trial: true, UpdateID: "upd-1", TargetVersion: version,
	})
	if hello := receive(t, parent, supervise.MsgHello); !hello.ReportsProgress || hello.Version != version {
		t.Fatalf("hello = %+v, want this version reporting progress", hello)
	}
	observe := sup.bootProgress()
	if observe == nil {
		t.Fatal("a trial has no boot progress observer")
	}
	observe(startupprogress.Progress{Phase: "store.migrate", Detail: "Applying migration 1 of 2", UpdatedAt: 1, AliveAt: 1})
	if got := receive(t, parent, supervise.MsgProgress); got.Progress == nil || got.Progress.Detail != "Applying migration 1 of 2" {
		t.Fatalf("progress = %+v, want the report", got)
	}

	sup.startFinished(errors.New("open the database: disk I/O error"))
	if failed := receive(t, parent, supervise.MsgFailed); failed.Reason != "open the database: disk I/O error" {
		t.Fatalf("failed = %+v, want Start's error", failed)
	}
	// A report after Start returned goes nowhere: the next frame on the
	// channel is the one sent after it. The pause is longer than an open
	// relay takes to deliver, so a report it forwarded would come first.
	observe(startupprogress.Progress{Phase: "store.migrate", Detail: "late", UpdatedAt: 2, AliveAt: 2})
	time.Sleep(100 * time.Millisecond)
	if err := sup.reportPrepared(); err != nil {
		t.Fatalf("send the marker frame: %v", err)
	}
	receive(t, parent, supervise.MsgPrepared)
}

// A trial that started sends no failed frame: prepared follows.
func TestAServeTrialThatStartedReportsNoFailure(t *testing.T) {
	sup, parent := attachAsChild(t, supervise.Message{
		Type: supervise.MsgActivate, ProtocolVersion: supervise.ProtocolVersion,
		Trial: true, UpdateID: "upd-1", TargetVersion: version,
	})
	receive(t, parent, supervise.MsgHello)
	sup.startFinished(nil)
	if err := sup.reportPrepared(); err != nil {
		t.Fatalf("reportPrepared: %v", err)
	}
	receive(t, parent, supervise.MsgPrepared)
}

// An ordinary supervised boot is not judged: it claims no progress, has no
// observer, and a failed start is not reported as a trial's.
func TestAnOrdinaryServeBootReportsNoProgress(t *testing.T) {
	sup, parent := attachAsChild(t, supervise.Message{
		Type: supervise.MsgActivate, ProtocolVersion: supervise.ProtocolVersion,
	})
	if hello := receive(t, parent, supervise.MsgHello); hello.ReportsProgress {
		t.Fatalf("hello = %+v, want no progress claimed outside a trial", hello)
	}
	if sup.bootProgress() != nil {
		t.Fatal("an ordinary boot forwards its progress")
	}
	sup.startFinished(errors.New("open the database: disk I/O error"))
	if err := sup.reportPrepared(); err != nil {
		t.Fatalf("send the marker frame: %v", err)
	}
	receive(t, parent, supervise.MsgPrepared)
}
