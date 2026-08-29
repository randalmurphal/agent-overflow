package harnessclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// readLimit matches transport.DefaultReadLimit. A ListItems response for
// a long thread runs into tens of MiB, and coder/websocket's default cap
// is 32 KiB — a client that left it there would fail on exactly the
// reads an assertion wants.
const readLimit = 75 * 1024 * 1024

type pendingCall struct {
	// method is carried so a failure names what failed: the read loop
	// sees only the correlation id.
	method string
	result chan json.RawMessage
	fail   chan error
}

// Client is one WebSocket connection to a harness instance: RPC by
// method name, plus the event log every wait and count reads.
type Client struct {
	conn *websocket.Conn

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[string]*pendingCall
	replays map[string]chan struct{}
	// replayChannels marks channels whose replay backlog is in flight. Live
	// frames may overtake that backlog on the wire, so sequence observations
	// must use a high-water cursor until the replay completion marker arrives.
	replayChannels  map[string]map[string]struct{}
	replaySequences map[string]map[string]*replaySequence
	log             []logEntry
	logCap          int
	waiters         map[int]*waiter
	listeners       map[int]func(Event)
	nextHook        int
	closeErr        error
	closed          bool
	sequences       map[string]channelSequence
	seqGaps         []SequenceGap
	seqFaults       []SequenceFault
	capMu           sync.Mutex
	capOnce         sync.Once
	capReady        bool
	cap             HarnessCapabilities
	capErr          error

	readDone chan struct{}
}

// CheckCapabilities performs the one handshake for this connection and
// caches its result. A failed handshake is cached too, so an older backend
// cannot be hammered once per command while the CLI is producing diagnostics.
func (c *Client) CheckCapabilities(ctx context.Context) (HarnessCapabilities, error) {
	c.capOnce.Do(func() {
		caps, err := c.Capabilities(ctx)
		c.capMu.Lock()
		c.cap, c.capErr, c.capReady = caps, err, true
		c.capMu.Unlock()
	})
	c.capMu.Lock()
	caps, err := cloneHarnessCapabilities(c.cap), c.capErr
	c.capMu.Unlock()
	return caps, err
}

// CachedCapabilities returns the result of CheckCapabilities. It refuses
// before a handshake so callers cannot accidentally treat a zero catalog as
// a compatible backend.
func (c *Client) CachedCapabilities() (HarnessCapabilities, error) {
	c.capMu.Lock()
	defer c.capMu.Unlock()
	if !c.capReady {
		return HarnessCapabilities{}, errors.New("harnessclient: capabilities handshake has not completed")
	}
	return cloneHarnessCapabilities(c.cap), c.capErr
}

func cloneHarnessCapabilities(caps HarnessCapabilities) HarnessCapabilities {
	caps.Methods = append([]string(nil), caps.Methods...)
	caps.Meters = append([]string(nil), caps.Meters...)
	caps.Actions = append([]string(nil), caps.Actions...)
	caps.Queries = append([]string(nil), caps.Queries...)
	caps.Workloads = append([]string(nil), caps.Workloads...)
	return caps
}

// Options tunes a connection. The zero value is what every caller wants.
type Options struct {
	// EventLogCap overrides how many events are retained for waits and
	// counts. Zero means defaultEventLogCap.
	EventLogCap int
}

// Dial connects to the instance a bootstrap payload describes.
func Dial(ctx context.Context, bs Bootstrap, opts Options) (*Client, error) {
	if bs.Port == 0 || bs.Token == "" {
		return nil, errors.New("harnessclient: bootstrap carries no port or token")
	}
	return DialURL(ctx, bs.WSURL(), opts)
}

// DialURL connects to an explicit ws:// endpoint, token included. Tests
// use it against a fake server; production callers go through Dial.
func DialURL(ctx context.Context, wsURL string, opts Options) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		// The server answers a bad token with 404 (deliberately
		// indistinguishable from "no such path"), so say both things a
		// reader might need rather than guessing which happened.
		return nil, fmt.Errorf("connect %s: %w (a 404 here means the token was refused or the instance is gone)", redactToken(wsURL), err)
	}
	conn.SetReadLimit(readLimit)
	logCap := opts.EventLogCap
	if logCap <= 0 {
		logCap = defaultEventLogCap
	}
	c := &Client{
		conn:            conn,
		pending:         map[string]*pendingCall{},
		replays:         map[string]chan struct{}{},
		replayChannels:  map[string]map[string]struct{}{},
		replaySequences: map[string]map[string]*replaySequence{},
		waiters:         map[int]*waiter{},
		listeners:       map[int]func(Event){},
		sequences:       make(map[string]channelSequence),
		logCap:          logCap,
		readDone:        make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Close tears the connection down. Pending calls fail rather than hang.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	err := c.conn.Close(websocket.StatusNormalClosure, "client closing")
	<-c.readDone
	return err
}

// Call invokes a backend method by exported name. Harness* methods and
// every bound App method share one namespace on this wire.
func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	raw := make([]json.RawMessage, 0, len(params))
	for i, p := range params {
		encoded, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("%s: encode param %d: %w", method, i, err)
		}
		raw = append(raw, encoded)
	}
	return c.CallRaw(ctx, method, raw)
}

// CallRaw is Call with parameters already encoded — the shape a CLI has,
// which takes its arguments as JSON text and must not re-encode them.
func (c *Client) CallRaw(ctx context.Context, method string, params []json.RawMessage) (json.RawMessage, error) {
	call := &pendingCall{method: method, result: make(chan json.RawMessage, 1), fail: make(chan error, 1)}

	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		if err == nil {
			err = errors.New("connection closed")
		}
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	c.nextID++
	id := "ao-harness-" + strconv.Itoa(c.nextID)
	c.pending[id] = call
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(ctx, clientFrame{Type: frameTypeRPC, ID: id, Method: method, Params: params}); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	case err := <-call.fail:
		return nil, err
	case result := <-call.result:
		return result, nil
	}
}

// CallInto is Call with the result decoded into out.
func (c *Client) CallInto(ctx context.Context, out any, method string, params ...any) error {
	raw, err := c.Call(ctx, method, params...)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}

// Subscribe narrows this connection to the named channels. Ordinary
// clients omit it and keep receiving every visible channel; a tail on
// one channel uses it so an unrelated streaming burst does not fill the
// event log out from under a wait.
func (c *Client) Subscribe(ctx context.Context, channels ...string) error {
	return c.write(ctx, clientFrame{Type: frameTypeSubscribe, Channels: channels})
}

// Replay asks the server to re-push events the client missed, per
// channel cursor, and returns when the completion marker arrives. Live
// events interleave with the replayed ones, which is why the marker
// exists at all.
func (c *Client) Replay(ctx context.Context, lastSeqByChannel map[string]uint64) error {
	done := make(chan struct{})
	c.mu.Lock()
	c.nextID++
	id := "ao-harness-replay-" + strconv.Itoa(c.nextID)
	c.replays[id] = done
	if c.replayChannels == nil {
		c.replayChannels = make(map[string]map[string]struct{})
	}
	channels := make(map[string]struct{}, len(lastSeqByChannel))
	sequences := make(map[string]*replaySequence, len(lastSeqByChannel))
	for channel := range lastSeqByChannel {
		channels[channel] = struct{}{}
		sequences[channel] = &replaySequence{baseline: lastSeqByChannel[channel], observed: make(map[uint64]struct{})}
	}
	c.replayChannels[id] = channels
	if c.replaySequences == nil {
		c.replaySequences = make(map[string]map[string]*replaySequence)
	}
	c.replaySequences[id] = sequences
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.replays, id)
		c.clearReplayChannelsLocked(id)
		delete(c.replaySequences, id)
		c.mu.Unlock()
	}()

	if err := c.write(ctx, clientFrame{Type: frameTypeReplay, ID: id, LastSeqByChannel: lastSeqByChannel}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("replay: %w", ctx.Err())
	case <-done:
		return nil
	}
}

// Done is closed when the read loop exits, whatever ended it.
func (c *Client) Done() <-chan struct{} { return c.readDone }

func (c *Client) write(ctx context.Context, frame clientFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode %s frame: %w", frame.Type, err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return fmt.Errorf("write %s frame: %w", frame.Type, err)
	}
	return nil
}

func (c *Client) readLoop() {
	defer close(c.readDone)
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.failAll(err)
			return
		}
		var frame serverFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			// A frame we cannot parse is a contract break, not a transient:
			// fail loudly rather than silently dropping wire traffic.
			c.failAll(fmt.Errorf("decode server frame: %w", err))
			_ = c.conn.Close(websocket.StatusProtocolError, "undecodable frame")
			return
		}
		switch frame.Type {
		case frameTypeRPC:
			c.completeCall(frame)
		case frameTypeEvent:
			c.dispatch(Event{Channel: frame.Channel, Seq: frame.Seq, Data: frame.Data, Gap: frame.Gap})
		case frameTypeBatch:
			for _, entry := range frame.Events {
				c.dispatch(Event{Channel: entry.Channel, Seq: entry.Seq, Data: entry.Data, Gap: entry.Gap})
			}
		case frameTypeReplay:
			c.completeReplay(frame.ID)
		case frameTypePing:
			// Keepalive. Its arrival is a liveness signal and nothing else.
		}
	}
}

func (c *Client) completeCall(frame serverFrame) {
	c.mu.Lock()
	call := c.pending[frame.ID]
	delete(c.pending, frame.ID)
	c.mu.Unlock()
	if call == nil {
		return
	}
	if frame.Error != nil {
		call.fail <- &RPCError{Method: call.method, Code: frame.Error.Code, Message: frame.Error.Message}
		return
	}
	call.result <- frame.Result
}

func (c *Client) completeReplay(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if done, ok := c.replays[id]; ok {
		c.reconcileReplayLocked(id)
		delete(c.replays, id)
		c.clearReplayChannelsLocked(id)
		delete(c.replaySequences, id)
		close(done)
		return
	}
	// A server that echoed no id (or one we already retired) still
	// completed SOMETHING; release every outstanding replay rather than
	// leaving a caller parked forever on a marker it cannot recognize.
	for key, done := range c.replays {
		c.reconcileReplayLocked(key)
		delete(c.replays, key)
		c.clearReplayChannelsLocked(key)
		delete(c.replaySequences, key)
		close(done)
	}
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	if c.closeErr == nil {
		c.closeErr = err
	}
	c.closed = true
	pending := c.pending
	c.pending = map[string]*pendingCall{}
	replays := c.replays
	c.replays = map[string]chan struct{}{}
	c.replayChannels = map[string]map[string]struct{}{}
	c.replaySequences = map[string]map[string]*replaySequence{}
	c.mu.Unlock()
	for _, call := range pending {
		call.fail <- fmt.Errorf("%s: %w", call.method, err)
	}
	for _, done := range replays {
		close(done)
	}
}

func (c *Client) clearReplayChannelsLocked(id string) {
	delete(c.replayChannels, id)
}

// redactToken strips the token from a URL for error messages: a failed
// connect gets printed, and a token in a terminal scrollback outlives
// the failure.
func redactToken(rawURL string) string {
	at := strings.Index(rawURL, "token=")
	if at < 0 {
		return rawURL
	}
	return rawURL[:at] + "token=<redacted>"
}
