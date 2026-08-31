package wsllauncher

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/selfupdate"
	"agent-overflow/internal/webview2host"
)

// notificationBridgeRPCTimeout bounds one call over the launcher's bridge
// connection. Every RPC the launcher makes answers a local, already-decided
// question on the backend side, so a call that has not been answered inside
// this window is a stalled connection, not slow work.
const notificationBridgeRPCTimeout = 5 * time.Second

// RPC id prefixes. Every call draws from one counter, so the prefix is
// cosmetic correlation help in a packet capture, not a uniqueness mechanism.
const (
	rpcIDPrefixActivation  = "notification"
	rpcIDPrefixUpdate      = "update"
	rpcIDPrefixShutdown    = "shutdown"
	rpcIDPrefixBrowserHost = "browserhost"
)

type notificationRPCResult struct {
	err error
}

// pendingRPC is one in-flight call. The method name rides along so a server
// error or a disconnect names the call that failed instead of assuming it was
// an activation.
type pendingRPC struct {
	method string
	result chan notificationRPCResult
}

// RPCRefusedError reports that the backend ANSWERED a call and rejected it.
//
// That distinction is load-bearing for the install handshake: a refusal is
// proof the call did not take effect, while a timeout or a disconnect is
// ambiguous — the backend may have acted on a call whose response we lost.
// Every server-answered error lands here, whether it is a semantic refusal
// (method_error — e.g. no install in flight, stale version) or a protocol
// mismatch from version skew (method_not_found, bad_params); in all of those
// cases the call provably did nothing.
type RPCRefusedError struct {
	Method  string
	Code    string
	Message string
}

// Shutdown asks an isolated WSL backend to begin its authenticated graceful
// shutdown. The response is only the backend control acknowledgement. Callers
// must still wait for the transport to disappear before closing their process
// containment or reusing the data root.
func (c *NotificationClient) Shutdown(ctx context.Context) error {
	return c.callRPC(ctx, rpcIDPrefixShutdown, "HarnessShutdown", nil)
}

func (e *RPCRefusedError) Error() string {
	return fmt.Sprintf("%s RPC %s: %s", e.Method, e.Code, e.Message)
}

// Activate posts the validated target back to the backend's local-only
// NotificationActivated RPC and waits for its response.
func (c *NotificationClient) Activate(ctx context.Context, target notify.Target) error {
	if err := notify.ValidateTarget(target); err != nil {
		return err
	}
	params, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("encode notification activation target: %w", err)
	}
	return c.callRPC(ctx, rpcIDPrefixActivation, "NotificationActivated", []json.RawMessage{params})
}

// ReportUpdateInstallStatus tells the backend how the launcher acted on an
// install directive: selfupdate.StatusProceeding once the staged file is
// confirmed and the swap is starting (which cancels the backend's ACK
// timeout), or selfupdate.StatusFailed with a reason when the launcher stays
// alive on the old version.
//
// The backend refuses a report that no longer matches its own state — its ACK
// deadline already unwound the install, or the version names a stale directive
// — and those arrive here as *RPCRefusedError. Callers acting on a proceeding
// report must branch on that (see ClassifyInstallAck): the backend has already
// told the user the install failed, so swapping anyway would contradict it.
//
// Bounded END TO END at the RPC timeout, connection wait included — unlike
// Activate, whose connection wait deliberately rides the caller's context so a
// cold-boot toast click survives the bridge still connecting. A directive only
// arrives over a live connection, so a bridge that is down now just died, and
// the backend's ACK deadline is already counting; blocking here until a
// reconnect would hold the launcher's install guard indefinitely and could
// land the report after that deadline, turning the designed fast-fail
// (timeout → plain error → ClassifyInstallAck reads it as undelivered) into a
// late refusal that aborts a swap the user asked for. The timeout staying
// under the backend's ACK deadline is what keeps that classification honest.
func (c *NotificationClient) ReportUpdateInstallStatus(ctx context.Context, stage, version, message string) error {
	ctx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
	defer cancel()
	params := make([]json.RawMessage, 0, 3)
	for _, value := range []string{stage, version, message} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode update install status: %w", err)
		}
		params = append(params, encoded)
	}
	return c.callRPC(ctx, rpcIDPrefixUpdate, selfupdate.RPCReportStatus, params)
}

// ReportBrowserHost tells the backend how the launcher's pane host acted:
// a controller was created (detail carries its CDP targetId), a create
// failed, a page closed, or a browser/renderer process died under one.
//
// Bounded END TO END at the RPC timeout, connection wait included, for
// the same reason as ReportUpdateInstallStatus: a report only exists
// because a directive arrived over a live connection, so a bridge that is
// down now just died, and blocking a UI-thread-adjacent handler until it
// returns would stall every later directive behind it. A lost report
// costs the backend one page handle it re-derives on the next directive
// round trip.
//
// The kind is checked here rather than trusted: a typo in launcher code
// would otherwise reach the backend as an unrecognised report, which it
// can only drop.
func (c *NotificationClient) ReportBrowserHost(ctx context.Context, pageID string, kind webview2host.ReportKind, detail string) error {
	if err := webview2host.ValidatePageID(pageID); err != nil {
		return err
	}
	if !webview2host.ValidKind(kind) {
		return fmt.Errorf("browser host report kind %q is not one the backend understands", kind)
	}
	ctx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
	defer cancel()
	params := make([]json.RawMessage, 0, 3)
	for _, value := range []string{pageID, string(kind), webview2host.TruncateDetail(detail)} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode browser host report: %w", err)
		}
		params = append(params, encoded)
	}
	return c.callRPC(ctx, rpcIDPrefixBrowserHost, webview2host.RPCReport, params)
}

// callRPC posts one method call over the live bridge connection and waits for
// its response. One pending map, one timeout, one disconnect story for every
// RPC the launcher makes. A server-answered rejection returns
// *RPCRefusedError; every other failure (no connection, write error, timeout)
// returns a plain error, because none of them prove the call did not land.
func (c *NotificationClient) callRPC(ctx context.Context, idPrefix, method string, params []json.RawMessage) error {
	conn, err := c.waitForConnection(ctx)
	if err != nil {
		return err
	}

	id := fmt.Sprintf("%s-%d", idPrefix, c.nextRPC.Add(1))
	result := make(chan notificationRPCResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = pendingRPC{method: method, result: result}
	c.pendingMu.Unlock()

	rpcCtx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
	defer cancel()
	if err := c.writeJSON(rpcCtx, conn, notificationClientFrame{
		Type:   "rpc",
		ID:     id,
		Method: method,
		Params: params,
	}); err != nil {
		c.removePending(id)
		return fmt.Errorf("%w: write %s RPC: %v", ErrNotificationBridgeDisconnected, method, err)
	}

	select {
	case <-rpcCtx.Done():
		c.removePending(id)
		return fmt.Errorf("%s RPC: %w", method, rpcCtx.Err())
	case response := <-result:
		return response.err
	}
}

func (c *NotificationClient) resolveRPC(frame notificationServerFrame) {
	c.pendingMu.Lock()
	call, ok := c.pending[frame.ID]
	delete(c.pending, frame.ID)
	c.pendingMu.Unlock()
	if !ok {
		return
	}
	if frame.Error != nil {
		call.result <- notificationRPCResult{err: &RPCRefusedError{
			Method:  call.method,
			Code:    frame.Error.Code,
			Message: frame.Error.Message,
		}}
		return
	}
	call.result <- notificationRPCResult{}
}

func (c *NotificationClient) removePending(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *NotificationClient) failPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]pendingRPC)
	c.pendingMu.Unlock()
	for _, call := range pending {
		call.result <- notificationRPCResult{err: err}
	}
}
