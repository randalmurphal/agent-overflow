package transport

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sync"

	"github.com/coder/websocket"
)

// DefaultReadLimit caps a single inbound message size. A long thread's
// ListRecentThreadItems response is the worst-case shape — items.meta
// + payload metadata across hundreds of turns can run into tens of
// MiB. 75 MiB is the headroom budget: large enough that real threads
// load without surprise, small enough to keep a hostile/buggy peer
// from exhausting host memory with a single frame. The frontend
// MAX_FRAME_BYTES tracks this value 1:1 so the symmetric cap holds —
// see frontend/src/lib/transport/wsClient.ts. Anything larger than
// this still belongs on a separate paged endpoint, not over WS.
const DefaultReadLimit = 75 * 1024 * 1024

// DefaultMaxConcurrentRPCs caps how many RPC dispatches a single WS
// connection can have in flight at once. Bound exists so a misbehaving
// or malicious client can't fan out unbounded goroutines on the
// server. Sized for typical streaming UX (chat thread expansion can
// fire ~30 GetPayloadData in parallel).
const DefaultMaxConcurrentRPCs = 64

// connProfile captures per-connection transport policy, determined once
// at upgrade time from the peer's RemoteAddr. Immutable for the
// connection's lifetime. Compression is negotiated separately at
// upgrade time via websocket.AcceptOptions.
type connProfile struct {
	// isLoopback records whether the peer is on a loopback interface.
	// Threaded into dispatcher origin checks and event visibility.
	isLoopback bool

	// coalesceEvents enables server-side event coalescing for this
	// connection. True for non-loopback peers; false for loopback
	// (where the overhead exceeds the benefit on a shared-memory pipe).
	coalesceEvents bool
}

// connHandler owns one upgraded WebSocket. It pumps client frames into
// the dispatcher and event subscriber back into the wire. Each handler
// runs to completion when the WS closes; the parent server waits via
// the WaitGroup so a graceful Shutdown can drain in-flight responses.
type connHandler struct {
	ws         *websocket.Conn
	dispatcher *Dispatcher
	bus        *EventBus
	sub        *Subscriber

	profile connProfile

	// rpcSem caps concurrent in-flight handleRPC goroutines per conn.
	// Acquired before `go handleRPC`, released in the goroutine's defer.
	rpcSem chan struct{}

	// rpcWG tracks in-flight RPC goroutines so connection teardown can
	// wait for responses to finish writing before closing the WS.
	rpcWG sync.WaitGroup

	// writeMu serialises writes to the WS. Both the request handler
	// goroutines and the event-pump goroutine share the connection;
	// concurrent writes corrupt the frame stream.
	writeMu sync.Mutex
}

// runConnHandler runs the per-connection lifecycle until the client
// disconnects or ctx is cancelled. Blocks until done.
//
// profile is captured at upgrade time and carries per-connection
// transport policy (loopback status, event coalescing).
// profile.isLoopback is forwarded to every dispatcher resolution so
// LocalOnlyMethods gets enforced for non-loopback peers.
func runConnHandler(ctx context.Context, ws *websocket.Conn, d *Dispatcher, bus *EventBus, readLimit int64, maxConcurrent int, profile connProfile) {
	if readLimit <= 0 {
		readLimit = DefaultReadLimit
	}
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrentRPCs
	}
	ws.SetReadLimit(readLimit)

	h := &connHandler{
		ws:         ws,
		dispatcher: d,
		bus:        bus,
		sub:        bus.Subscribe(),
		profile:    profile,
		rpcSem:     make(chan struct{}, maxConcurrent),
	}
	defer h.sub.Close()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Per-connection scratch space. Handlers (e.g. GitStatusSubscribe)
	// register cleanup callbacks here so a dropped WS releases their
	// long-lived server-side resources without waiting for the App to
	// shut down. runCleanups runs in LIFO order after the read loop
	// exits and after in-flight RPC goroutines drain.
	connCtx, state := WithConnState(connCtx)
	defer state.RunCleanups()

	// Event pump: deliver every event the bus produces to the wire.
	// Lives on its own goroutine so a slow read doesn't backpressure
	// the publisher (the EventBus already drops on full subscriber
	// channels).
	go h.pumpEvents(connCtx)

	h.readLoop(connCtx)

	// Wait for in-flight RPC handlers to finish writing their
	// responses before we let the parent close the WS underneath them.
	h.rpcWG.Wait()
}

// readLoop processes inbound frames until the client closes or an
// unrecoverable error occurs. RPC responses are dispatched on a
// bounded worker pool; replay frames pull from the bus and stream
// missed events synchronously before the live pump resumes.
func (h *connHandler) readLoop(ctx context.Context) {
	for {
		mt, data, err := h.ws.Read(ctx)
		if err != nil {
			// Normal closure (StatusNormalClosure, StatusGoingAway,
			// NoStatusRcvd) is not an error. Other read errors get
			// logged at INFO. AbnormalClosure (1006) IS noteworthy —
			// it can indicate a network drop or misbehaving peer, so
			// we log it.
			if !isClosedError(err) {
				log.Printf("transport: ws read: %v", err)
			}
			return
		}
		if mt != websocket.MessageText {
			h.writeError(ctx, "", &FrameError{
				Code:    ErrCodeBadParams,
				Message: "binary frames not supported",
			})
			continue
		}

		var frame ClientFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			h.writeError(ctx, "", &FrameError{
				Code:    ErrCodeBadParams,
				Message: "malformed frame",
			})
			continue
		}

		switch frame.Type {
		case frameTypeRPC:
			h.dispatchRPC(ctx, frame)
		case frameTypeReplay:
			h.handleReplay(ctx, frame)
		default:
			h.writeError(ctx, frame.ID, &FrameError{
				Code:    ErrCodeBadParams,
				Message: "unknown frame type",
			})
		}
	}
}

// dispatchRPC enforces the per-conn concurrency cap and spawns a
// handler goroutine. A blocked semaphore acquire waits for an
// in-flight RPC to finish — back-pressuring the read loop instead of
// queueing unbounded goroutines.
func (h *connHandler) dispatchRPC(ctx context.Context, frame ClientFrame) {
	select {
	case h.rpcSem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	h.rpcWG.Go(func() {
		defer func() { <-h.rpcSem }()
		h.handleRPC(ctx, frame)
	})
}

// handleRPC invokes the registered method (by ID first, falling back to
// name) and writes the response. Errors from the dispatcher already
// arrive as FrameError values — we don't need to translate.
//
// ResolveForOrigin enforces LocalOnlyMethods against the per-conn
// isLoopback flag. A non-loopback peer attempting a privileged method
// gets ErrCodeMethodNotFound (matching an unregistered method) rather
// than a distinct forbidden code, so the privileged surface stays
// unenumerable from the LAN.
func (h *connHandler) handleRPC(ctx context.Context, frame ClientFrame) {
	method, fe := h.dispatcher.ResolveForOrigin(frame.MethodID, frame.Method, h.profile.isLoopback)
	if fe != nil {
		h.writeError(ctx, frame.ID, fe)
		return
	}

	result, fe := h.dispatcher.InvokeForOrigin(ctx, method, frame.Params, h.profile.isLoopback)
	if fe != nil {
		h.writeError(ctx, frame.ID, fe)
		return
	}

	h.writeFrame(ctx, ServerFrame{
		Type:   frameTypeRPC,
		ID:     frame.ID,
		Result: result,
	})
}

// handleReplay streams every event missed since the client's
// LastSeqByChannel into the wire. Live deliveries continue from the
// event pump in parallel; any event whose seq <= the last replayed
// seq from the live pump is naturally suppressed by the client's own
// dedup (the wsClient lib tracks lastSeq per channel).
//
// The map size is capped so a malicious replay request can't force
// the bus to allocate proportionally large response slices.
func (h *connHandler) handleReplay(ctx context.Context, frame ClientFrame) {
	if len(frame.LastSeqByChannel) > MaxReplayChannels {
		h.writeError(ctx, "", &FrameError{
			Code:    ErrCodeBadParams,
			Message: "replay map too large",
		})
		return
	}
	missed := h.bus.Replay(frame.LastSeqByChannel)
	for _, e := range missed {
		if !eventVisibleToOrigin(e.Channel, h.profile.isLoopback) {
			continue
		}
		h.writeEventFrame(ctx, e)
	}
}

// pumpEvents copies live events from the subscriber into the wire.
// Returns when ctx is cancelled or the subscriber's Done channel
// closes (bus shutdown / Subscriber.Close).
//
// Loopback connections dispatch each event immediately (no overhead
// on a shared-memory pipe). Remote connections accumulate events in
// a coalescing buffer and flush as a single batch frame on a timer
// or count threshold, reducing TCP framing and write-syscall cost.
func (h *connHandler) pumpEvents(ctx context.Context) {
	if !h.profile.coalesceEvents {
		h.pumpEventsImmediate(ctx)
		return
	}
	h.pumpEventsCoalesced(ctx)
}

func (h *connHandler) pumpEventsImmediate(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.sub.Done():
			return
		case e := <-h.sub.Events():
			if !eventVisibleToOrigin(e.Channel, h.profile.isLoopback) {
				continue
			}
			h.writeEventFrame(ctx, e)
		}
	}
}

func (h *connHandler) pumpEventsCoalesced(ctx context.Context) {
	buf := newCoalesceBuffer(DefaultCoalesceMaxEvents, DefaultCoalesceWindow, func(batch []Event) {
		h.writeBatchFrame(ctx, batch)
	})
	defer buf.stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-h.sub.Done():
			return
		case <-buf.timerC():
			buf.flushNow()
		case e := <-h.sub.Events():
			if !eventVisibleToOrigin(e.Channel, h.profile.isLoopback) {
				continue
			}
			buf.add(e)
		}
	}
}

// writeBatchFrame writes a coalesced batch of events as a single wire
// frame. When the batch contains exactly one event, it falls through
// to writeEventFrame which uses the pre-encoded WireBytes fast path,
// avoiding a redundant json.Marshal.
func (h *connHandler) writeBatchFrame(ctx context.Context, events []Event) {
	if len(events) == 0 {
		return
	}
	if len(events) == 1 {
		h.writeEventFrame(ctx, events[0])
		return
	}
	entries := make([]batchEventEntry, len(events))
	for i, e := range events {
		entries[i] = batchEventEntry{
			Channel: e.Channel,
			Seq:     e.Seq,
			Data:    e.Data,
			Gap:     e.Gap,
		}
	}
	frame := batchFrame{
		Type:   frameTypeBatch,
		Events: entries,
	}
	buf, err := json.Marshal(frame)
	if err != nil {
		log.Printf("transport: marshal batch frame: %v", err)
		return
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if err := h.ws.Write(ctx, websocket.MessageText, buf); err != nil {
		if !isClosedError(err) {
			log.Printf("transport: ws write batch: %v", err)
		}
	}
}

// writeEventFrame is the event-only fast path: when WireBytes is
// available (Emit pre-encodes for live fanout, Replay pre-encodes
// gap markers) the writer skips marshal entirely. The fallback
// re-marshals the envelope so a future code path that produces an
// Event without pre-encoding still gets a valid wire frame.
func (h *connHandler) writeEventFrame(ctx context.Context, e Event) {
	if len(e.WireBytes) > 0 {
		h.writeMu.Lock()
		defer h.writeMu.Unlock()
		if err := h.ws.Write(ctx, websocket.MessageText, e.WireBytes); err != nil {
			if !isClosedError(err) {
				log.Printf("transport: ws write: %v", err)
			}
		}
		return
	}
	h.writeFrame(ctx, ServerFrame{
		Type:    frameTypeEvent,
		Channel: e.Channel,
		Seq:     e.Seq,
		Data:    e.Data,
		Gap:     e.Gap,
	})
}

// writeFrame marshals + writes a frame under the write lock. Errors
// from the WS are logged but not returned — the read loop will see
// the closed connection and exit.
func (h *connHandler) writeFrame(ctx context.Context, frame ServerFrame) {
	buf, err := json.Marshal(frame)
	if err != nil {
		log.Printf("transport: marshal server frame: %v", err)
		return
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if err := h.ws.Write(ctx, websocket.MessageText, buf); err != nil {
		if !isClosedError(err) {
			log.Printf("transport: ws write: %v", err)
		}
	}
}

func (h *connHandler) writeError(ctx context.Context, id string, fe *FrameError) {
	h.writeFrame(ctx, ServerFrame{
		Type:  frameTypeRPC,
		ID:    id,
		Error: fe,
	})
}

// isClosedError reports whether err represents a normal client
// disconnect or context cancellation. AbnormalClosure (1006) is NOT
// in this set — it can indicate a network drop, so it gets logged.
func isClosedError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure ||
		status == websocket.StatusGoingAway ||
		status == websocket.StatusNoStatusRcvd
}

// upgrade authenticates the request via ?token= query param and
// upgrades to WebSocket. Returns the connection on success or writes
// the appropriate HTTP status on failure.
//
// enableCompression negotiates permessage-deflate with context
// takeover when true. Intended for non-loopback connections where
// the wire cost justifies the per-connection flate memory (~1.5 MB).
// Loopback connections skip compression — shared-memory pipe, no
// benefit. Clients that don't support permessage-deflate (Safari /
// WKWebView) fall back to uncompressed transparently.
func upgrade(w http.ResponseWriter, r *http.Request, expectedToken string, originPatterns []string, enableCompression bool) (*websocket.Conn, error) {
	supplied := r.URL.Query().Get("token")
	if err := ConstantTimeEqual(expectedToken, supplied); err != nil {
		// Match the unauth path's response shape with the static asset
		// 404. Distinguishable status codes let a LAN scanner fingerprint
		// "this is the agent-overflow server" — return 404 instead.
		http.NotFound(w, r)
		return nil, err
	}
	opts := &websocket.AcceptOptions{}
	if enableCompression {
		opts.CompressionMode = websocket.CompressionContextTakeover
	}
	if len(originPatterns) == 0 {
		// Loopback-only: skip origin checks. The token itself is the
		// gate, and there's no LAN-attached browser-origin to validate.
		opts.InsecureSkipVerify = true
	} else {
		opts.OriginPatterns = originPatterns
	}
	return websocket.Accept(w, r, opts)
}

// remoteAddrIsLoopback reports whether the peer's RemoteAddr is a
// loopback interface. Used at upgrade time to decide whether to allow
// LocalOnlyMethods for the resulting connection.
//
// Three parse paths:
//
//  1. netip.ParseAddrPort handles the canonical "ip:port" form
//     (e.g. "127.0.0.1:54321", "[::1]:54321"). The IsLoopback method
//     understands every loopback variant — IPv4 127/8, IPv6 ::1, and
//     IPv4-mapped-in-IPv6 ::ffff:127.0.0.1.
//  2. net.SplitHostPort + netip.ParseAddr is the fallback for inputs
//     that ParseAddrPort rejects (rare — typically synthetic test
//     requests with malformed addresses).
//  3. An unparseable RemoteAddr is treated as non-loopback (fail
//     closed). httptest sometimes leaves RemoteAddr empty; the
//     production transport always populates it. Defaulting to "not
//     loopback" means a synthetic request can't inadvertently bypass
//     LocalOnly enforcement just because its RemoteAddr was malformed.
//
// Note: this trusts that the kernel reports a true peer address. A
// reverse-proxy fronting the transport would have to terminate WS
// upgrades and re-issue them with a real loopback peer for the
// LocalOnlyMethods enforcement to remain meaningful — that's the
// documented deployment model for v1.
func remoteAddrIsLoopback(remoteAddr string) bool {
	if remoteAddr == "" {
		return false
	}
	if addrPort, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return addrPort.Addr().IsLoopback()
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}
