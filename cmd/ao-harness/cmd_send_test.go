package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"agent-overflow/internal/cdpclient"
	"agent-overflow/internal/harnessclient"
	"agent-overflow/internal/transport"
)

// attachScripted points a test env at a scripted backend whose catalog
// lists methods and whose one thread is thread-1. answer, when not nil,
// takes each method it returns a result for.
func attachScripted(t *testing.T, answer func(method string) any, methods ...string) (*env, *bytes.Buffer, *scriptedBackend) {
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
		default:
			return "ok"
		}
	})
	root := t.TempDir()
	writeInstanceFile(t, root, deadPID(t), func(bs *harnessclient.Bootstrap) {
		bs.Port = port
		bs.Token = token
	})
	e, stdout, _ := testEnv(t.TempDir())
	e.instance = root
	return e, stdout, backend
}

// An instance that takes the send but lacks a call the command makes after
// it is refused before anything is sent: a retry of a command that failed
// after its send would send the message again under a fresh send id.
func TestSendWaitItemsChecksEveryCallBeforeSending(t *testing.T) {
	e, _, backend := attachScripted(t, nil, harnessclient.SendMethod, "HarnessListThreadRows")
	err := runSend(e, []string{"--thread", "thread-1", "--wait", "--items", "--timeout", "2s", "hello"})
	if err == nil || !strings.Contains(err.Error(), listItemsMethod) {
		t.Fatalf("send --wait --items against an instance without %s: %v", listItemsMethod, err)
	}
	if slices.Contains(backend.called(), harnessclient.SendMethod) {
		t.Fatalf("the message was sent before the refusal: calls %v", backend.called())
	}
}

// `items` is a diagnostic: it reads the rows of an instance whose
// capability handshake fails, as it did before the handshake existed.
func TestItemsReadsAnInstanceWhoseHandshakeFails(t *testing.T) {
	e, stdout, _ := attachScripted(t, func(method string) any {
		switch method {
		case "HarnessCapabilities":
			return "not a capabilities document"
		case listItemsMethod:
			return []itemRow{{ID: "user:1", TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed", Summary: "hello from the old instance"}}
		}
		return nil
	})
	if err := runItems(e, []string{"--thread", "thread-1"}); err != nil {
		t.Fatalf("items after a failed handshake: %v", err)
	}
	if !strings.Contains(stdout.String(), "hello from the old instance") {
		t.Fatalf("items printed %q, want the thread's row", stdout.String())
	}
}

// A working thread refuses profile's send after the profiler and the
// scenario are armed. The profile fails with the refusal, says what it left
// behind, and stops the profiler.
func TestProfileOfAWorkingThreadStopsTheProfiler(t *testing.T) {
	const refusal = "send message: placement: thread is working"
	e, _, backend := attachScripted(t, func(method string) any {
		if method == harnessclient.SendMethod {
			return &transport.FrameError{Code: transport.ErrCodeMethodError, Message: refusal}
		}
		return nil
	}, harnessclient.SendMethod)
	devtools := newRecordingDevtools(t)
	err := e.withClient(context.Background(), func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		_, err := profileOneTurn(context.Background(), e, client, devtools.conn, profileTurn{
			threadID: "thread-1", scenario: "probe", message: "profile probe", intervalUs: 100, timeout: time.Second,
		})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), refusal) || !strings.Contains(err.Error(), "scenario probe is installed and the profiler stopped") {
		t.Fatalf("profile of a working thread: %v", err)
	}
	if !slices.Contains(backend.called(), "HarnessSetScenario") {
		t.Fatalf("calls %v, want the scenario installed before the send", backend.called())
	}
	if methods := devtools.methods(); len(methods) == 0 || methods[len(methods)-1] != "Profiler.stop" {
		t.Fatalf("debugger calls %v, want the profiler stopped last", methods)
	}
}

// recordingDevtools is a debugger target that answers every call with an
// empty result and records the methods called.
type recordingDevtools struct {
	conn   *cdpclient.Conn
	mu     sync.Mutex
	called []string
}

func newRecordingDevtools(t *testing.T) *recordingDevtools {
	t.Helper()
	devtools := &recordingDevtools{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var call struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(data, &call); err != nil {
				t.Errorf("undecodable debugger call %s: %v", data, err)
				return
			}
			devtools.mu.Lock()
			devtools.called = append(devtools.called, call.Method)
			devtools.mu.Unlock()
			reply, _ := json.Marshal(map[string]any{"id": call.ID, "result": map[string]any{}})
			if err := conn.Write(r.Context(), websocket.MessageText, reply); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	conn, err := cdpclient.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	devtools.conn = conn
	return devtools
}

func (d *recordingDevtools) methods() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.called...)
}
