package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"github.com/google/uuid"
)

// remoteWaitFixture pairs a source with a destination whose injected runner
// writes a banner and a long body, then blocks until released or canceled.
type remoteWaitFixture struct {
	source   *App
	receiver *pairedBackend
	peerID   string
	project  store.Project
	thread   store.Thread
	endpoint string
	release  chan struct{}
}

func newRemoteWaitFixture(t *testing.T) *remoteWaitFixture {
	t.Helper()
	f := &remoteWaitFixture{receiver: newPairedBackend(t), source: identityApp(t), release: make(chan struct{})}
	f.source.configDir = t.TempDir()
	manager, err := attachedbackends.New(t.TempDir(), "source", "test")
	if err != nil {
		t.Fatal(err)
	}
	f.source.backends = manager
	t.Cleanup(func() { _ = f.source.remoteMCPServer().Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	invite, _ := f.receiver.mintLink(t, "full")
	peer, err := manager.Add(ctx, invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.peerID = peer.ID
	if err = f.receiver.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err = manager.Await(ctx, peer.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.source.SetAgentComputerEnabled(ctx, peer.ID, true); err != nil {
		t.Fatal(err)
	}
	f.project, err = f.receiver.app.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "target", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	f.receiver.app.remoteJobs, err = remotejobs.New(context.Background(), f.receiver.app.store, func(ctx context.Context, _ string, _ []string, out io.Writer) (remotejobs.Outcome, error) {
		_, _ = io.WriteString(out, "banner line\n"+strings.Repeat("x", 12000)+"\n")
		select {
		case <-f.release:
			_, _ = io.WriteString(out, "done line\n")
			return remotejobs.Outcome{ExitCode: 0}, nil
		case <-ctx.Done():
			return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.receiver.app.remoteJobs.Close)
	var token string
	f.thread, token = remoteMCPThread(t, f.source, string(provider.Codex))
	f.endpoint = remoteMCPEndpoint(t, f.source, f.thread, token)
	return f
}

func (f *remoteWaitFixture) run(t *testing.T, id string, wait float64) remoteMCPResult {
	t.Helper()
	args := map[string]any{"computer_id": f.peerID, "project_id": f.project.ID, "request_id": id, "argv": []string{"test-helper"}, "wait_seconds": wait}
	var receipt remoteMCPResult
	if err := json.Unmarshal(remoteMCPCall(t, f.endpoint, "remote_run", args, false), &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

// status starts a parked remote_status call and reports its result on the
// returned channel once the reply has been written.
func (f *remoteWaitFixture) status(t *testing.T, id string, wait float64) <-chan remoteMCPResult {
	t.Helper()
	// A call releases its wait just after it writes its reply, so the
	// remote_run before this one may still be registered on the job. Wait
	// for that release, or the registration check below would see it in
	// place of this call's.
	deadline := time.Now().Add(5 * time.Second)
	for f.source.remoteWaitActive(f.peerID, id) {
		if time.Now().After(deadline) {
			t.Fatal("an earlier wait on the job never released")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan remoteMCPResult, 1)
	go func() {
		var receipt remoteMCPResult
		raw := remoteMCPCall(t, f.endpoint, "remote_status", map[string]any{"computer_id": f.peerID, "request_id": id, "wait_seconds": wait}, false)
		if err := json.Unmarshal(raw, &receipt); err != nil {
			t.Error(err)
		}
		done <- receipt
	}()
	deadline = time.Now().Add(5 * time.Second)
	for !f.source.remoteWaitActive(f.peerID, id) {
		if time.Now().After(deadline) {
			t.Fatal("wait never registered")
		}
		time.Sleep(time.Millisecond)
	}
	return done
}

func (f *remoteWaitFixture) watch(t *testing.T, id string) store.RemoteWatch {
	t.Helper()
	w, err := f.source.store.GetRemoteWatch(f.peerID, id)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// A run that outlasts its wait returns a backgrounded receipt with both ends
// of the output; the completion then arrives once, as a message, carrying the
// same excerpt shape.
func TestRemoteRunBackgroundsAfterWaitAndCompletionArrivesAsMessage(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	f := newRemoteWaitFixture(t)
	id := uuid.NewString()
	receipt := f.run(t, id, 0.3)
	if receipt.State != "running" || !receipt.Backgrounded || receipt.Notification != "pending" || !strings.Contains(receipt.BackgroundHint, "background") {
		t.Fatalf("not backgrounded: %+v", receipt)
	}
	if !strings.HasPrefix(receipt.OutputHead, "banner line\n") || !strings.HasSuffix(receipt.Output, "x\n") || receipt.OmittedOutputBytes == 0 || !strings.Contains(receipt.OutputHint, "outputHead") {
		t.Fatalf("backgrounded reply lacks head and tail: head=%q tail=%d omitted=%d hint=%q", receipt.OutputHead, len(receipt.Output), receipt.OmittedOutputBytes, receipt.OutputHint)
	}
	if w := f.watch(t, id); w.Notification != "pending" {
		t.Fatalf("backgrounded job lost its watch: %+v", w)
	}
	close(f.release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		job, err := f.receiver.app.store.GetRemoteJob(id)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("destination did not settle: %+v", job)
		}
		time.Sleep(time.Millisecond)
	}
	f.source.checkRemoteWatch(f.watch(t, id))
	rows := durableQueueRows(t, f.source, f.thread.ID)
	if len(rows) != 1 {
		t.Fatalf("completion queue: %+v", rows)
	}
	message := rows[0].Message
	for _, want := range []string{"Result: succeeded", "Output start (untrusted):\nbanner line", "Output end (untrusted):", "done line", "Output omitted", "remote_fetch_log"} {
		if !strings.Contains(message, want) {
			t.Fatalf("completion missing %q: %s", want, message)
		}
	}
	f.source.checkRemoteWatch(f.watch(t, id))
	if rows = durableQueueRows(t, f.source, f.thread.ID); len(rows) != 1 {
		t.Fatalf("completion delivered twice: %+v", rows)
	}
	// The whole retained log copies to a local file, and the submitting
	// conversation keeps that access after the destination is opted out.
	if err := f.source.SetAgentComputerEnabled(context.Background(), f.peerID, false); err != nil {
		t.Fatal(err)
	}
	var fetched RemoteLogArtifact
	if err := json.Unmarshal(remoteMCPCall(t, f.endpoint, "remote_fetch_log", map[string]any{"computer_id": f.peerID, "request_id": id}, false), &fetched); err != nil {
		t.Fatal(err)
	}
	expected := "banner line\n" + strings.Repeat("x", 12000) + "\ndone line\n"
	actual, err := os.ReadFile(fetched.Path)
	digest := sha256.Sum256([]byte(expected))
	if err != nil || string(actual) != expected || fetched.SHA256 != hex.EncodeToString(digest[:]) || fetched.Name != id+".log" || fetched.Size != int64(len(expected)) || fetched.Log.TotalBytes != int64(len(expected)) || fetched.Log.Truncated || fetched.ComputerID != f.peerID || fetched.RequestID != id {
		t.Fatalf("log copy=%+v bytes=%d err=%v", fetched, len(actual), err)
	}
}

// A reply that carried the finished result is the delivery: the watcher
// leaves a job alone while a call is parked on it, and the watch is dismissed
// once that call has answered, so no completion message follows.
func TestRemoteWaitReplyIsTheOnlyDelivery(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	f := newRemoteWaitFixture(t)
	id := uuid.NewString()
	if receipt := f.run(t, id, 0.2); receipt.State != "running" {
		t.Fatalf("expected a running receipt: %+v", receipt)
	}
	done := f.status(t, id, 5)
	f.source.checkRemoteWatch(f.watch(t, id))
	if w := f.watch(t, id); w.Notification != "pending" || w.Receipt.State != "running" {
		t.Fatalf("watcher acted on a job with a parked call: %+v", w)
	}
	close(f.release)
	select {
	case receipt := <-done:
		if receipt.State != "succeeded" || receipt.Backgrounded || receipt.Notification != "dismissed" || !strings.Contains(receipt.Output, "done line") || !strings.HasPrefix(receipt.OutputHead, "banner line") {
			t.Fatalf("parked call reply: %+v", receipt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked call never returned")
	}
	deadline := time.Now().Add(3 * time.Second)
	for f.watch(t, id).Notification != "dismissed" {
		if time.Now().After(deadline) {
			t.Fatalf("delivered reply did not dismiss the watch: %+v", f.watch(t, id))
		}
		time.Sleep(time.Millisecond)
	}
	f.source.checkRemoteWatch(f.watch(t, id))
	if rows := durableQueueRows(t, f.source, f.thread.ID); len(rows) != 0 {
		t.Fatalf("result delivered twice: %+v", rows)
	}
	if items, err := f.source.remoteTrayItems(f.thread.ID, 0); err != nil || len(items) != 0 {
		t.Fatalf("dismissed job still in the tray: %+v %v", items, err)
	}
}

// replyWriter is the client end of one reply: onWrite runs as the reply is
// written and decides what the client did with it.
type replyWriter struct {
	*httptest.ResponseRecorder
	onWrite func() error
}

func (w *replyWriter) Write(p []byte) (int, error) {
	if err := w.onWrite(); err != nil {
		return 0, err
	}
	return w.ResponseRecorder.Write(p)
}

// statusThrough parks a remote_status call on the job through the MCP
// transport, settles the job, and returns once the call has finished,
// including the work it deferred until its reply went out. accept is the
// client's Accept header; both provider CLIs list text/event-stream.
func (f *remoteWaitFixture) statusThrough(t *testing.T, ctx context.Context, id string, w http.ResponseWriter, accept string) {
	t.Helper()
	for f.source.remoteWaitActive(f.peerID, id) {
		time.Sleep(time.Millisecond)
	}
	endpoint, err := url.Parse(f.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": "remote_status", "arguments": map[string]any{"computer_id": f.peerID, "request_id": id, "wait_seconds": 5},
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, endpoint.Path, bytes.NewReader(body))
	request.RemoteAddr = "[::1]:40000"
	request.Header.Set("Content-Type", "application/json")
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		f.source.remoteMCPServer().ServeHTTP(w, request)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !f.source.remoteWaitActive(f.peerID, id) {
		if time.Now().After(deadline) {
			t.Fatal("wait never registered")
		}
		time.Sleep(time.Millisecond)
	}
	close(f.release)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("parked call never returned")
	}
}

// Whether the settled reply reached the client decides who delivers the
// result, not whether the client is still connected afterwards. A client
// that hangs up once it holds the reply has received it, so the watch is
// dismissed; a reply that could not be written leaves the watch to deliver
// the result once.
func TestRemoteWaitReplyDeliveryIsTheTransportsAnswer(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	t.Run("read then hung up", func(t *testing.T) {
		t.Parallel()
		f := newRemoteWaitFixture(t)
		id := uuid.NewString()
		if receipt := f.run(t, id, 0.2); receipt.State != "running" {
			t.Fatalf("expected a running receipt: %+v", receipt)
		}
		ctx, hangUp := context.WithCancel(context.Background())
		defer hangUp()
		w := &replyWriter{ResponseRecorder: httptest.NewRecorder(), onWrite: func() error {
			// The client takes the bytes and leaves before the handler
			// returns.
			defer hangUp()
			return nil
		}}
		f.statusThrough(t, ctx, id, w, "")
		if ctx.Err() == nil || !strings.Contains(w.Body.String(), `\"notification\":\"dismissed\"`) {
			t.Fatalf("reply was not the settled receipt read before the hang-up: %s", w.Body.String())
		}
		if watch := f.watch(t, id); watch.Notification != "dismissed" {
			t.Fatalf("a reply the client read did not dismiss the watch: %+v", watch)
		}
		f.source.checkRemoteWatch(f.watch(t, id))
		if rows := durableQueueRows(t, f.source, f.thread.ID); len(rows) != 0 {
			t.Fatalf("result delivered twice: %+v", rows)
		}
	})
	t.Run("write failed", func(t *testing.T) {
		t.Parallel()
		f := newRemoteWaitFixture(t)
		id := uuid.NewString()
		if receipt := f.run(t, id, 0.2); receipt.State != "running" {
			t.Fatalf("expected a running receipt: %+v", receipt)
		}
		w := &replyWriter{ResponseRecorder: httptest.NewRecorder(), onWrite: func() error {
			return errors.New("broken pipe")
		}}
		f.statusThrough(t, context.Background(), id, w, "")
		if watch := f.watch(t, id); watch.Notification != "pending" || watch.Receipt.State != "succeeded" {
			t.Fatalf("an undelivered reply settled the watch: %+v", watch)
		}
		f.source.checkRemoteWatch(f.watch(t, id))
		rows := durableQueueRows(t, f.source, f.thread.ID)
		if len(rows) != 1 || !strings.Contains(rows[0].Message, "Result: succeeded") {
			t.Fatalf("the watcher did not deliver the result once: %+v", rows)
		}
	})
}

// On the event-stream path both provider CLIs use, the watch is dismissed
// before the wait ends: a watcher pass landing between the two finds nothing
// owed, so a client that read the reply and hung up gets no second copy.
func TestRemoteReplyIsDismissedBeforeItsWaitEnds(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	f := newRemoteWaitFixture(t)
	id := uuid.NewString()
	if receipt := f.run(t, id, 0.2); receipt.State != "running" {
		t.Fatalf("expected a running receipt: %+v", receipt)
	}
	landed := false
	f.source.remoteReplySettledForTest = func() {
		landed = true
		watch, err := f.source.store.GetRemoteWatch(f.peerID, id)
		if err != nil {
			t.Error(err)
			return
		}
		f.source.checkRemoteWatch(watch)
	}
	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	w := &replyWriter{ResponseRecorder: httptest.NewRecorder(), onWrite: func() error { return nil }}
	f.statusThrough(t, ctx, id, w, "application/json, text/event-stream")
	hangUp()
	if !landed || !strings.Contains(w.Body.String(), "event: message") || !strings.Contains(w.Body.String(), `\"notification\":\"dismissed\"`) {
		t.Fatalf("not a settled event-stream reply (seam ran %v): %s", landed, w.Body.String())
	}
	if watch := f.watch(t, id); watch.Notification != "dismissed" {
		t.Fatalf("watch = %+v, want dismissed", watch)
	}
	f.source.checkRemoteWatch(f.watch(t, id))
	if rows := durableQueueRows(t, f.source, f.thread.ID); len(rows) != 0 {
		t.Fatalf("result delivered twice: %+v", rows)
	}
}

// The wait is bounded by the tool schema, and an interrupt ends a parked call
// at once with a backgrounded receipt while the command keeps running.
func TestRemoteWaitIsCappedAndInterruptEndsIt(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	f := newRemoteWaitFixture(t)
	id := uuid.NewString()
	tooLong := map[string]any{"computer_id": f.peerID, "project_id": f.project.ID, "request_id": id, "argv": []string{"test-helper"}, "wait_seconds": maxRemoteWaitSeconds + 1}
	if failure := string(remoteMCPCall(t, f.endpoint, "remote_run", tooLong, true)); !strings.Contains(failure, "wait_seconds") {
		t.Fatalf("wait above the cap accepted: %s", failure)
	}
	if receipt := f.run(t, id, 0.2); receipt.State != "running" {
		t.Fatalf("expected a running receipt: %+v", receipt)
	}
	done := f.status(t, id, 30)
	began := time.Now()
	f.source.cancelRemoteWaits(f.thread.ID)
	select {
	case receipt := <-done:
		if receipt.State != "running" || !receipt.Backgrounded || time.Since(began) > 5*time.Second {
			t.Fatalf("interrupted wait: %+v after %s", receipt, time.Since(began))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interrupt did not end the wait")
	}
	if job, err := f.receiver.app.store.GetRemoteJob(id); err != nil || job.State != "running" {
		t.Fatalf("interrupting the wait touched the command: %+v %v", job, err)
	}
	if w := f.watch(t, id); w.Notification != "pending" {
		t.Fatalf("interrupted wait dismissed the watch: %+v", w)
	}
}

// Provider ceilings sit above the longest wait, so a parked remote_run is
// AO's to answer rather than the provider's to abort or background.
func TestRemoteMCPProviderCeilingsExceedTheLongestWait(t *testing.T) {
	t.Parallel()
	if remoteMCPCallCeiling <= maxRemoteWaitSeconds*time.Second || defaultRemoteRunWaitSeconds > maxRemoteWaitSeconds {
		t.Fatalf("ceiling %s, wait cap %ds, default %ds", remoteMCPCallCeiling, maxRemoteWaitSeconds, defaultRemoteRunWaitSeconds)
	}
	base := map[string]any{"url": "http://127.0.0.1:1/mcp"}
	claude, _ := remoteMCPServerConfig(string(provider.Claude), base).(map[string]any)
	codex, _ := remoteMCPServerConfig(string(provider.Codex), base).(map[string]any)
	if claude["timeout"] != remoteMCPCallCeiling.Milliseconds() || claude["url"] != base["url"] || codex["tool_timeout_sec"] != int(remoteMCPCallCeiling.Seconds()) || codex["url"] != base["url"] {
		t.Fatalf("claude=%v codex=%v", claude, codex)
	}
	if _, set := base["timeout"]; set {
		t.Fatal("decoration mutated the shared config")
	}
	env := withRemoteMCPClaudeEnv(map[string]string{"HOME": "/h"})
	if env[claudeMCPAutoBackgroundEnvVar] != "1200000" || env["HOME"] != "/h" {
		t.Fatalf("claude env: %v", env)
	}
	if kept := withRemoteMCPClaudeEnv(map[string]string{claudeMCPAutoBackgroundEnvVar: "1"}); kept[claudeMCPAutoBackgroundEnvVar] != "1" {
		t.Fatalf("operator override lost: %v", kept)
	}
	for _, tool := range remoteToolDefinitions {
		if tool["name"] != "remote_run" {
			continue
		}
		properties := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		wait := properties["wait_seconds"].(map[string]any)
		if wait["default"] != defaultRemoteRunWaitSeconds || wait["maximum"] != maxRemoteWaitSeconds {
			t.Fatalf("remote_run wait schema: %v", wait)
		}
		if _, unlimited := properties["unlimited"]; unlimited {
			t.Fatal("unlimited remains in the tool schema")
		}
		if timeout := properties["timeout_seconds"].(map[string]any); timeout["default"] != 0 {
			t.Fatalf("timeout default: %v", timeout)
		}
	}
}
