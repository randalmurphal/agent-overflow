package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harnessclient"
)

// attachScripted points a test env at a scripted backend whose catalog
// lists methods and whose one thread is thread-1, at rest. answer, when not nil,
// takes each method it returns a result for.
func attachScripted(t *testing.T, answer func(method string) any, methods ...string) (*env, *scriptedBackend) {
	t.Helper()
	const token = "t"
	port, backend := startScriptedBackend(t, token, func(method string) any {
		if answer != nil {
			if result := answer(method); result != nil {
				return result
			}
		}
		switch method {
		case "HarnessCapabilities":
			return harnessclient.HarnessCapabilities{ProtocolRevision: cliHarnessProtocolRevision, Methods: methods}
		case "HarnessListThreadRows":
			return []threadRow{{ID: "thread-1", Title: "Probe"}}
		case harnessclient.ThreadWorkMethod:
			return []any{}
		default:
			return "ok"
		}
	})
	root := t.TempDir()
	writeInstanceFile(t, root, deadPID(t), func(bs *harnessclient.Bootstrap) {
		bs.Port = port
		bs.Token = token
	})
	e, _, _ := testEnv(t.TempDir())
	e.instance = root
	return e, backend
}

// An instance that takes the send but lacks a call the command makes after
// it is refused before anything is sent: a retry of a command that failed
// after its send would send the message again under a fresh send id.
func TestSendWaitItemsChecksEveryCallBeforeSending(t *testing.T) {
	e, backend := attachScripted(t, nil, harnessclient.SendMethod, "HarnessListThreadRows")
	err := runSend(e, []string{"--thread", "thread-1", "--wait", "--items", "--timeout", "2s", "hello"})
	if err == nil {
		t.Fatal("send --wait --items succeeded against an instance without the calls it ends with")
	}
	for _, missing := range []string{harnessclient.ThreadWorkMethod, listItemsMethod} {
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("refusal does not name %s: %v", missing, err)
		}
	}
	if slices.Contains(backend.called(), harnessclient.SendMethod) {
		t.Fatalf("the message was sent before the refusal: calls %v", backend.called())
	}
}

// `items` refuses an instance without the read with the protocol refusal
// rather than calling a method it does not have.
func TestItemsRefusesAnInstanceWithoutTheRead(t *testing.T) {
	e, backend := attachScripted(t, nil, "HarnessListThreadRows")
	err := runItems(e, []string{"--thread", "thread-1"})
	if err == nil || !strings.Contains(err.Error(), "harness protocol preflight") || !strings.Contains(err.Error(), listItemsMethod) {
		t.Fatalf("items against an instance without %s: %v", listItemsMethod, err)
	}
	if slices.Contains(backend.called(), listItemsMethod) {
		t.Fatalf("items called %s on an instance that lacks it: calls %v", listItemsMethod, backend.called())
	}
}

// `send --wait` to a working thread is refused before the send: the message
// would queue behind that work, and the completion the thread reports next
// would be the earlier turn's.
func TestSendWaitRefusesAWorkingThreadBeforeSending(t *testing.T) {
	e, backend := attachScripted(t, func(method string) any {
		if method == harnessclient.ThreadWorkMethod {
			return []map[string]any{{"threadId": "thread-1", "activeTurn": true}}
		}
		return nil
	}, harnessclient.SendMethod, harnessclient.ThreadWorkMethod, "HarnessListThreadRows")
	err := runSend(e, []string{"--thread", "thread-1", "--wait", "--timeout", "2s", "hello"})
	if err == nil || !strings.Contains(err.Error(), "is working (a turn is open)") {
		t.Fatalf("send --wait to a working thread: %v", err)
	}
	if slices.Contains(backend.called(), harnessclient.SendMethod) {
		t.Fatalf("the message was sent to a working thread: calls %v", backend.called())
	}
}

// `profile` refuses a working thread before it arms the profiler or
// installs the scenario, so the refusal leaves nothing to undo. The CDP
// connection is nil: anything armed before the check would use it.
func TestProfileRefusesAWorkingThreadBeforeArming(t *testing.T) {
	e, backend := attachScripted(t, func(method string) any {
		if method == harnessclient.ThreadWorkMethod {
			return []map[string]any{{"threadId": "thread-1", "pendingSends": true}}
		}
		return nil
	}, harnessclient.SendMethod, harnessclient.ThreadWorkMethod)
	err := e.withClient(context.Background(), func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		_, err := profileOneTurn(context.Background(), e, client, nil, profileTurn{
			threadID: "thread-1", scenario: "probe", message: "profile probe", intervalUs: 100, timeout: time.Second,
		})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "is working (a message is queued or starting its turn)") {
		t.Fatalf("profile of a working thread: %v", err)
	}
	for _, armed := range []string{"HarnessSetScenario", harnessclient.SendMethod} {
		if slices.Contains(backend.called(), armed) {
			t.Fatalf("profile called %s before refusing: calls %v", armed, backend.called())
		}
	}
}
