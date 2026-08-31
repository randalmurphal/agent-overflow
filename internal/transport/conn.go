package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

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

const (
	// defaultKeepaliveInterval is how often the per-connection keepalive
	// loop writes a client-visible {"type":"ping"} frame — full
	// rationale in docs/architecture/transport.md; the frontend
	// STALE_TRAFFIC_THRESHOLD_MS is 3× this cadence, keep them in
	// ratio. Config.KeepaliveInterval overrides (tests shrink it).
	defaultKeepaliveInterval = 10 * time.Second
	// Every keepalivePingEvery-th heartbeat tick additionally sends a
	// protocol-level ping and waits for the pong, detecting half-open
	// TCP (peer gone, no FIN — writes buffer silently) server-side.
	keepalivePingEvery = 3
	// defaultKeepalivePongTimeout bounds the pong wait. Generous vs the
	// ~ms loopback RTT: pong delivery needs the peer's read loop, and a
	// busy streaming client may lag. Config.KeepalivePongTimeout
	// overrides (tests shrink it).
	defaultKeepalivePongTimeout = 10 * time.Second
	// writeTimeout bounds every WS write. A half-open connection
	// accepts writes into a dead TCP window until the send buffer
	// fills, then blocks forever — holding writeMu and wedging the
	// event pump and the keepalive loop with it. On expiry
	// coder/websocket closes the connection (a frame can't be safely
	// abandoned mid-write), which is exactly the teardown that peer
	// needs. Sized for the largest legitimate frame (a tens-of-MiB
	// thread load) crossing a slow WAN link.
	writeTimeout = 30 * time.Second
)

// heartbeatFrame is the keepalive frame, encoded once from the same
// ServerFrame shape every other server frame uses so the wire string
// can never drift from frameTypePing.
var heartbeatFrame = func() []byte {
	buf, err := json.Marshal(ServerFrame{Type: frameTypePing})
	if err != nil {
		panic(fmt.Sprintf("transport: marshal heartbeat frame: %v", err))
	}
	return buf
}()

// connProfile captures per-connection transport policy, determined once
// at upgrade time from the peer's RemoteAddr. Immutable for the
// connection's lifetime. Compression is negotiated separately at
// upgrade time via websocket.AcceptOptions.
type connProfile struct {
	// isLoopback records whether the peer is on a loopback interface.
	// Threaded into dispatcher origin checks and event visibility.
	isLoopback bool
	// remoteAddr is the peer's kernel-reported address, carried for the
	// per-connection close log so concurrent clients' lines correlate.
	remoteAddr string
	// client is the screen on the other end, declared on the upgrade URL.
	// Zero when the client declared nothing, which is normal.
	client ClientIdentity
	// sessionID is the durable session this connection presented, resolved
	// before the upgrade by Config.SessionForRequest. Empty means the
	// connection names no session — every connection today, and still the
	// ordinary case for the local webview afterwards — and such a
	// connection is not tracked by the live-session registry.
	sessionID string
}

// connSettings carries the server-config knobs runConnHandler needs.
// Zero values take the package defaults.
type connSettings struct {
	readLimit         int64
	maxConcurrentRPCs int
	keepaliveInterval time.Duration
	pongTimeout       time.Duration
	// sessionConns is the server's live-session registry, or nil when the
	// handler runs outside one (unit tests). A connection naming a session
	// registers itself here so a revocation can reach it.
	sessionConns *SessionConns
	// hello is the frame written before any other traffic. Populated per
	// connection rather than once at boot because ServerTimeMs is the
	// clock at accept time — a value cached at startup would be a
	// confidently wrong answer to the one question the field exists to
	// settle.
	hello helloFrame
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

	// keepaliveInterval / pongTimeout are the resolved timing knobs
	// (connSettings with defaults applied).
	keepaliveInterval time.Duration
	pongTimeout       time.Duration

	// inRead is true exactly while the read loop sits in ws.Read.
	// coder/websocket only surfaces pongs from inside Read, so a
	// missing pong while the reader is off processing a frame
	// (streaming a replay, waiting on rpcSem) says nothing about the
	// peer — the keepalive loop skips its teardown verdict then.
	inRead atomic.Bool
	// revoked is set by closeForRevocation before it tears the socket
	// down, so the per-connection close line names the revocation rather
	// than reporting the context cancel as a server shutdown. One store
	// per revocation, one load per connection close.
	revoked atomic.Bool
	// lastReadAt (unix nanos) is stamped after each successful Read.
	// Second guard for the same race: a reader that re-entered Read
	// moments ago may not have had time to surface a pong yet.
	lastReadAt atomic.Int64
}

// runConnHandler runs the per-connection lifecycle until the client
// disconnects or ctx is cancelled. Blocks until done.
//
// profile is captured at upgrade time and carries per-connection
// transport policy. profile.isLoopback is forwarded to every
// dispatcher resolution so LocalOnlyMethods gets enforced for
// non-loopback peers.
func runConnHandler(ctx context.Context, ws *websocket.Conn, d *Dispatcher, bus *EventBus, settings connSettings, profile connProfile) {
	if settings.readLimit <= 0 {
		settings.readLimit = DefaultReadLimit
	}
	if settings.maxConcurrentRPCs <= 0 {
		settings.maxConcurrentRPCs = DefaultMaxConcurrentRPCs
	}
	if settings.keepaliveInterval <= 0 {
		settings.keepaliveInterval = defaultKeepaliveInterval
	}
	if settings.pongTimeout <= 0 {
		settings.pongTimeout = defaultKeepalivePongTimeout
	}
	ws.SetReadLimit(settings.readLimit)

	sub := bus.Subscribe()
	// Arm enqueue-time origin filtering before the pump starts:
	// channels this origin can never see must not consume the
	// subscriber's buffer slots during bursts (they'd force drops of
	// visible events and gap-driven re-fetches).
	sub.SetOriginLoopback(profile.isLoopback)
	h := &connHandler{
		ws:                ws,
		dispatcher:        d,
		bus:               bus,
		sub:               sub,
		profile:           profile,
		rpcSem:            make(chan struct{}, settings.maxConcurrentRPCs),
		keepaliveInterval: settings.keepaliveInterval,
		pongTimeout:       settings.pongTimeout,
	}
	h.lastReadAt.Store(time.Now().UnixNano())
	defer h.sub.Close()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Per-connection scratch space. Handlers (e.g. GitStatusSubscribe)
	// register cleanup callbacks here so a dropped WS releases their
	// long-lived server-side resources without waiting for the App to
	// shut down. runCleanups runs in LIFO order after the read loop
	// exits and after in-flight RPC goroutines drain.
	connCtx, state := WithConnState(connCtx, profile.client)
	defer state.RunCleanups()

	// A connection carrying a durable session joins the live-session
	// registry, so revoking that session force-closes this socket instead
	// of leaving it streaming under a credential the database says is
	// dead. Deregistration rides the SAME cleanup pass every other
	// per-connection resource uses, rather than a parallel teardown that
	// could disagree with it about when this connection ended.
	if detach := settings.sessionConns.attach(
		profile.sessionID, h.closeForRevocation(cancel),
	); !state.RegisterCleanup(detach) {
		// The connection was already closing when we got here. Nothing
		// will run the cleanup list again, so undo the attach ourselves.
		detach()
	}

	// Hello first, synchronously, before the pump and keepalive
	// goroutines exist. Ordering is the contract: a client that reads
	// hello as the first frame can seed its compatibility state before
	// the first event or RPC answer lands, and does not need a
	// "have I been told yet" branch on every other frame. Racing it
	// against the pump would make the guarantee probabilistic.
	//
	// A failed write means the peer is already gone. Nothing to recover:
	// return and let the deferred teardown run, rather than serving a
	// connection whose first frame never arrived.
	if err := h.writeHello(connCtx, settings.hello); err != nil {
		log.Printf("transport: ws %s hello write failed: %s", profile.remoteAddr, closeReason(err))
		return
	}

	// Event pump: deliver every event the bus produces to the wire.
	// Lives on its own goroutine so a slow read doesn't backpressure
	// the publisher (the EventBus already drops on full subscriber
	// channels).
	go h.pumpEvents(connCtx)

	// Keepalive: heartbeat frames + periodic protocol pings. Exits on
	// ctx cancel or the first failed write/pong (the read loop owns
	// teardown; on pong timeout the keepalive closes the conn itself
	// to unblock it).
	go h.keepalive(connCtx)

	started := time.Now()
	readErr := h.readLoop(connCtx)
	// One line per connection lifetime, graceful closes included — the
	// close signature (status vs raw error) is what distinguishes a
	// client navigation from a relay teardown or a network drop after
	// the fact, and suppressing graceful closes made the 2026-07-28
	// relay-flap diagnosis needlessly indirect.
	log.Printf("transport: ws %s closed after %s (loopback=%t): %s",
		profile.remoteAddr, time.Since(started).Round(time.Millisecond),
		profile.isLoopback, h.closeReason(readErr))

	// Wait for in-flight RPC handlers to finish writing their
	// responses before we let the parent close the WS underneath them.
	h.rpcWG.Wait()
}

// keepalive runs the per-connection heartbeat loop. Every tick writes
// the client-visible ping frame; every keepalivePingEvery-th tick also
// round-trips a protocol ping. A failed heartbeat write means the conn
// is already closed/closing — the read loop is handling teardown, so
// just exit. A pong timeout means the conn is half-open (writes still
// buffering into a dead TCP window): close it so the read loop exits
// and the client's reconnect takes over.
func (h *connHandler) keepalive(ctx context.Context) {
	ticker := time.NewTicker(h.keepaliveInterval)
	defer ticker.Stop()
	for tick := 1; ; tick++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := h.writeRaw(ctx, heartbeatFrame); err != nil {
			// Conn already closed/closing (or the write bound tore it
			// down) — the read loop owns teardown.
			return
		}
		if tick%keepalivePingEvery != 0 {
			continue
		}
		pingCtx, cancel := context.WithTimeout(ctx, h.pongTimeout)
		pingErr := h.ws.Ping(pingCtx)
		cancel()
		if pingErr == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		// A missing pong only convicts the peer when our own read loop
		// was in a position to surface one: pongs are processed inside
		// ws.Read, so a reader that is off processing a frame (or only
		// just returned to Read) leaves the pong undelivered on a
		// perfectly live connection. Skip the verdict and re-judge on a
		// later tick — a genuinely dead conn keeps the reader parked in
		// Read with an old lastReadAt.
		if !h.inRead.Load() || time.Since(time.Unix(0, h.lastReadAt.Load())) < h.pongTimeout {
			continue
		}
		if errors.Is(pingErr, context.DeadlineExceeded) {
			log.Printf("transport: keepalive: no pong within %s; closing connection", h.pongTimeout)
		} else if !isClosedError(pingErr) {
			log.Printf("transport: keepalive: ping failed: %v", pingErr)
		}
		_ = h.ws.CloseNow()
		return
	}
}

// closeForRevocation builds the teardown the live-session registry calls
// when this connection's session is revoked.
//
// Three steps, and the order is why it is not just ws.Close:
//
//  1. Close the event subscriber. The bus stops delivering to this
//     connection immediately, which is what makes "stops their event
//     streams synchronously" true at the moment CloseSession returns
//     rather than whenever the read loop happens to notice.
//  2. Cancel the connection context, stopping the pump and the keepalive.
//  3. CloseNow the socket, which unblocks the reader parked in ws.Read so
//     the ordinary teardown path runs and the cleanups fire.
//
// Every step is idempotent, because this races the connection's own
// close: Subscriber.close is a compare-and-swap, context cancel is
// idempotent by definition, and CloseNow on a closed connection is a
// no-op error nobody reads.
func (h *connHandler) closeForRevocation(cancel context.CancelFunc) func() {
	return func() {
		h.revoked.Store(true)
		h.sub.Close()
		cancel()
		_ = h.ws.CloseNow()
	}
}

// closeReason renders readLoop's terminal error (always non-nil — the
// loop only exits by returning a Read error) for the per-connection
// close log line. WS-level close statuses (1000 normal, 1001 going
// away, 1005 no-status/clean FIN, 1006 abnormal) are surfaced as their
// code — 1005 with no close frame is the signature of an intermediary
// (e.g. the WSL2 localhost relay) tearing the connection down, which a
// raw error string would obscure. The raw-error fallback is quoted and
// clamped: it can carry peer-influenced text.
func closeReason(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "server shutdown"
	}
	if status := websocket.CloseStatus(err); status != -1 {
		return fmt.Sprintf("close status %d", status)
	}
	return fmt.Sprintf("%.200q", err.Error())
}

// closeReason is closeReason plus the one thing the error cannot carry:
// a revoked connection is torn down by cancelling its context, which is
// indistinguishable from a server shutdown at the error alone. Reporting
// both the same way would make a revocation invisible in the log exactly
// when somebody is checking whether one took effect.
func (h *connHandler) closeReason(err error) string {
	if h.revoked.Load() {
		return "session revoked"
	}
	return closeReason(err)
}

// readLoop processes inbound frames until the client closes or an
// unrecoverable error occurs, returning the terminal read error for
// the caller's per-connection close line (which replaces the old
// abnormal-only inline logging). RPC responses are dispatched on a
// bounded worker pool; replay frames pull from the bus and stream
// missed events synchronously before the live pump resumes.
func (h *connHandler) readLoop(ctx context.Context) error {
	for {
		h.inRead.Store(true)
		mt, data, err := h.ws.Read(ctx)
		h.inRead.Store(false)
		if err != nil {
			return err
		}
		h.lastReadAt.Store(time.Now().UnixNano())
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
		case frameTypeSubscribe:
			h.handleSubscribe(ctx, frame)
		default:
			h.writeError(ctx, frame.ID, &FrameError{
				Code:    ErrCodeBadParams,
				Message: "unknown frame type",
			})
		}
	}
}

func (h *connHandler) handleSubscribe(ctx context.Context, frame ClientFrame) {
	if len(frame.Channels) == 0 || len(frame.Channels) > MaxSubscribeChannels {
		h.writeError(ctx, frame.ID, &FrameError{Code: ErrCodeBadParams, Message: "invalid event subscription"})
		return
	}
	for _, channel := range frame.Channels {
		if channel == "" || len(channel) > 256 {
			h.writeError(ctx, frame.ID, &FrameError{Code: ErrCodeBadParams, Message: "invalid event subscription"})
			return
		}
	}
	h.sub.SetChannels(frame.Channels)
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
// Replay ships through the same batch frames the live pump uses, in
// chunks of DefaultCoalesceMaxEvents. A reconnect during heavy
// streaming is exactly the moment the client can least afford
// per-event macrotasks: the ring holds up to DefaultRingCapacity
// (1000) events, so the un-batched loop handed the worst case the
// least protection. No timer is involved — the whole backlog is
// already in hand, so chunking is a pure fan-in with no added latency.
// Ordering survives: writeBatchFrame writes each chunk in order under
// writeMu, spliceBatchFrame preserves slice order inside a chunk, and
// every batch consumer iterates entries in order.
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
	// One reusable chunk: writeBatchFrame serialises into fresh bytes
	// before returning, so nothing retains the Event slice afterwards.
	chunk := make([]Event, 0, min(len(missed), DefaultCoalesceMaxEvents))
	for _, e := range missed {
		if !eventVisibleToOrigin(e.Channel, h.profile.isLoopback) {
			continue
		}
		if !h.sub.accepts(e.Channel) {
			continue
		}
		chunk = append(chunk, e)
		if len(chunk) == DefaultCoalesceMaxEvents {
			h.writeBatchFrame(ctx, chunk)
			chunk = chunk[:0]
		}
	}
	// Trailing partial chunk; writeBatchFrame no-ops on an empty slice
	// and falls through to a plain event frame for a single event.
	h.writeBatchFrame(ctx, chunk)
	// Replay and the live pump share writeMu, so live frames may interleave
	// with replay frames. This completion marker lets clients buffer the one
	// channel whose strict click ordering matters, then apply by sequence.
	h.writeFrame(ctx, ServerFrame{Type: frameTypeReplay, ID: frame.ID})
}

// pumpEvents copies live events from the subscriber into the wire.
// Returns when ctx is cancelled or the subscriber's Done channel
// closes (bus shutdown / Subscriber.Close).
//
// Every connection accumulates events in a coalescing buffer and
// flushes as a single batch frame on a timer or count threshold.
// For remote peers that reduces TCP framing and write-syscall cost;
// for the loopback webview the win is on the receiving side — one
// frame per 16ms window means one macrotask, one JSON.parse, and one
// reactive effect flush for a burst that previously arrived as dozens
// of individual messages competing with the render loop (the
// streaming-jank investigation, 2026-06-12). Sparse traffic is
// unaffected: a single-event window falls through to a regular
// `type:"event"` frame with at most one window of added latency.
func (h *connHandler) pumpEvents(ctx context.Context) {
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
			// Correctness gate. Enqueue-time filtering (Subscriber
			// SetOriginLoopback) already keeps invisible frames out of
			// the buffer; this backstop covers any event enqueued
			// before the filter was armed.
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
// avoiding a redundant json.Marshal. Multi-event windows splice the
// same pre-encoded envelopes instead of re-passing every payload
// through the JSON encoder — exactly the windows that dominate during
// streaming bursts.
func (h *connHandler) writeBatchFrame(ctx context.Context, events []Event) {
	if len(events) == 0 {
		return
	}
	if len(events) == 1 {
		h.writeEventFrame(ctx, events[0])
		return
	}
	buf := spliceBatchFrame(events)
	if len(buf) == 0 {
		return
	}
	if err := h.writeRaw(ctx, buf); err != nil {
		if !isClosedError(err) {
			log.Printf("transport: ws write batch: %v", err)
		}
	}
}

// spliceBatchFrame assembles {"type":"batch","events":[...]} by joining
// each event's pre-encoded WireBytes with commas. Every WireBytes is a
// complete {"type":"event",channel,seq,data,gap?} object, and all batch
// consumers (wsClient handleFrame, the wsllauncher notification client,
// the e2e harness dispatcher) read only channel/seq/data/gap from each
// entry, so the inert extra "type" field is tolerated — the one wire
// difference vs the retired per-batch re-marshal. Events lacking
// WireBytes are practically absent (Emit always pre-encodes, and
// Replay pre-encodes its gap markers) but are envelope-encoded
// defensively so a splice never emits a hole.
func spliceBatchFrame(events []Event) []byte {
	size := len(batchFramePrefix) + len(events) + 1 // separators + "]}"
	for i := range events {
		size += len(events[i].WireBytes)
	}
	buf := make([]byte, 0, size)
	buf = append(buf, batchFramePrefix...)
	written := 0
	for i := range events {
		wire := events[i].WireBytes
		if len(wire) == 0 {
			var err error
			if wire, err = encodeEventFrame(events[i]); err != nil {
				log.Printf("transport: encode batch entry: %v", err)
				continue
			}
		}
		if written > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, wire...)
		written++
	}
	if written == 0 {
		return nil
	}
	return append(buf, ']', '}')
}

// writeEventFrame is the event-only fast path: when WireBytes is
// available (Emit pre-encodes for live fanout, Replay pre-encodes
// gap markers) the writer skips marshal entirely. The fallback
// re-marshals the envelope so a future code path that produces an
// Event without pre-encoding still gets a valid wire frame.
func (h *connHandler) writeEventFrame(ctx context.Context, e Event) {
	if len(e.WireBytes) > 0 {
		if err := h.writeRaw(ctx, e.WireBytes); err != nil {
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

// writeFrame marshals + writes a frame. Errors from the WS are logged
// but not returned — the read loop will see the closed connection and
// exit.
func (h *connHandler) writeFrame(ctx context.Context, frame ServerFrame) {
	buf, err := json.Marshal(frame)
	if err != nil {
		log.Printf("transport: marshal server frame: %v", err)
		return
	}
	if err := h.writeRaw(ctx, buf); err != nil {
		if !isClosedError(err) {
			log.Printf("transport: ws write: %v", err)
		}
	}
}

// writeHello writes the connection's opening frame. Unlike writeFrame it
// RETURNS the error: hello is the one frame whose failure is worth
// abandoning the connection over, since everything after it assumes the
// client has been told what it is talking to.
func (h *connHandler) writeHello(ctx context.Context, hello helloFrame) error {
	hello.Type = frameTypeHello
	hello.ProtocolVersion = ProtocolVersion
	if hello.Capabilities == nil {
		// Never `null` on the wire: an empty array says "advertises
		// nothing", which a client reads without a nil check, while null
		// invites one more branch at every consumer.
		hello.Capabilities = []string{}
	}
	buf, err := json.Marshal(hello)
	if err != nil {
		return fmt.Errorf("marshal hello frame: %w", err)
	}
	return h.writeRaw(ctx, buf)
}

// writeRaw is the single wire-write chokepoint: pre-encoded bytes go
// out under the write lock with the shared write bound. A write that
// exhausts writeTimeout means the peer stopped draining (half-open TCP
// with a full send window) — coder/websocket tears the connection down
// on the expired context, so writeMu can never be held hostage by a
// dead peer. That case is logged here, once, so callers' isClosedError
// suppression (the expiry surfaces as context.DeadlineExceeded)
// doesn't bury the one write failure that carries a diagnosis.
func (h *connHandler) writeRaw(ctx context.Context, buf []byte) error {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	h.writeMu.Lock()
	err := h.ws.Write(wctx, websocket.MessageText, buf)
	h.writeMu.Unlock()
	if err != nil && wctx.Err() != nil && ctx.Err() == nil {
		log.Printf("transport: ws write timed out after %s; peer not draining — closing connection", writeTimeout)
	}
	return err
}

func (h *connHandler) writeError(ctx context.Context, id string, fe *FrameError) {
	h.writeFrame(ctx, ServerFrame{
		Type:  frameTypeRPC,
		ID:    id,
		Error: fe,
	})
}

// isClosedError reports whether a write error represents a peer that
// already disconnected normally (or a cancelled context) — cases the
// write paths suppress rather than log, because the read loop's close
// line already covers them. AbnormalClosure (1006) is NOT in this set:
// it can indicate a network drop mid-write, so it gets logged.
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

// upgrade validates the request and upgrades it to a WebSocket.
// Returns the connection on success, or writes the HTTP refusal and
// returns the error.
//
// Two gates, in order. The origin check first: a handshake carrying an
// Origin outside what this listener serves is refused before its
// credential is read, because a browser attaches the page cookie to
// such a handshake whether or not the page that opened it could ever
// read that cookie (OriginAllowed explains why another port on the same
// host is not a different cookie scope). Then the credential itself,
// through the same Credential.Authenticate every other route uses: the
// page cookie for a browser, the session token for a client that is not
// one.
//
// enableCompression negotiates permessage-deflate with context
// takeover when true. Intended for non-loopback connections where
// the wire cost justifies the per-connection flate memory (~1.5 MB).
// Loopback connections skip compression — shared-memory pipe, no
// benefit. Clients that don't support permessage-deflate (Safari /
// WKWebView) fall back to uncompressed transparently.
func upgrade(w http.ResponseWriter, r *http.Request, cred *Credential, originPatterns []string, enableCompression bool) (*websocket.Conn, error) {
	if !OriginAllowed(r, originPatterns) {
		// Same 404 as a refused credential and as a path that does not
		// exist, so no response shape tells one apart from the others.
		http.NotFound(w, r)
		return nil, errOriginNotServed
	}
	if !cred.Authenticate(r) {
		// Match the unauth path's response shape with the static asset
		// 404. Distinguishable status codes let a LAN scanner fingerprint
		// "this is the agent-overflow server" — return 404 instead.
		http.NotFound(w, r)
		return nil, errCredentialRefused
	}
	opts := &websocket.AcceptOptions{
		// The origin decision is made above, against the live allow-list
		// and this request's own authority, and it answers with this
		// package's 404 rather than the library's 403. Leaving the
		// library's own check on as well would mean two policies to keep
		// in agreement, one of them writing a fingerprintable status.
		InsecureSkipVerify: true,
	}
	if enableCompression {
		opts.CompressionMode = websocket.CompressionContextTakeover
	}
	return websocket.Accept(w, r, opts)
}

// errOriginNotServed and errCredentialRefused name the two handshake
// refusals for the caller's own logging. Neither reaches the wire: both
// answer the same 404.
var (
	errOriginNotServed   = errors.New("transport: request origin is not served by this listener")
	errCredentialRefused = errors.New("transport: request carries no valid credential")
)
