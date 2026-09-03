package wsllauncher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/relaysession"
	"agent-overflow/internal/selfupdate"
	"agent-overflow/internal/webview2host"

	"github.com/coder/websocket"
)

var ErrNotificationBridgeDisconnected = errors.New("notification bridge disconnected")

const notificationBridgeReadLimit = 1024 * 1024

type NotificationClientConfig struct {
	WSURL      string
	Token      string
	Present    func(notify.Send) error
	Logf       func(string, ...any)
	MinBackoff time.Duration
	MaxBackoff time.Duration

	// HandleUpdateInstall, when non-nil, additionally subscribes this
	// connection to selfupdate.ChannelInstall and hands every well-formed
	// directive to the callback. Leaving it nil keeps the client's wire
	// behavior exactly as it was before self-update existed: one subscribed
	// channel, one replay cursor.
	//
	// The callback runs on its own goroutine, not the read loop: the launcher
	// answers a directive by posting an RPC back over this same connection,
	// and that response can only arrive if the read loop is free.
	HandleUpdateInstall func(selfupdate.InstallDirective)

	// HandleWebviewTrim, when non-nil, additionally subscribes this
	// connection to the ephemeral webview:trim directive channel and hands
	// every frame to the callback with its reason string. Same posture as
	// HandleUpdateInstall: nil keeps the wire unchanged, the callback runs
	// off the read loop, and the channel carries no replay cursor — a trim
	// is only meaningful in the idle moment it was emitted.
	HandleWebviewTrim func(reason string)

	// HandleKeepAwake, when non-nil, additionally subscribes this
	// connection to the power:keepawake directive channel and hands every
	// well-formed frame's mode ("off" | "system" | "display") to the
	// callback. Same nil-keeps-the-wire-unchanged posture as the two
	// above, with one difference that matters:
	//
	// This channel carries a LEVEL, not an edge. The launcher owns the
	// Win32 execution state for the whole machine, so after a reconnect it
	// must end up holding whatever the backend's CURRENT setting says —
	// not whatever it happened to hold before the drop. The channel is
	// RetentionLatestOnly on the server, and this client asks for it with
	// a replay cursor pinned at ZERO on every connection, so the newest
	// frame is redelivered whether it was emitted at the backend's boot an
	// hour ago or lands a moment from now. Convergence therefore needs no
	// re-emit on subscribe and no cursor bookkeeping here.
	HandleKeepAwake func(mode string)

	// HandleBrowserHost, when non-nil, additionally subscribes this
	// connection to the ephemeral browser:host directive channel and
	// hands every VALID directive to the callback. Same posture as
	// HandleWebviewTrim: nil keeps the wire unchanged, the callback runs
	// off the read loop, and the channel carries no replay cursor — a
	// pane directive speaks for the layout it was emitted into, and
	// replaying a backlog would reopen pages the user has closed.
	//
	// Validation happens HERE, not in the handler: a directive names a
	// profile that becomes a directory on disk and a page the launcher
	// creates OS windows for, so an invalid one is logged and dropped
	// and never reaches the host — the same trust-boundary rule the
	// install directive follows.
	HandleBrowserHost func(directive webview2host.Directive)
}

// NotificationClient is the launcher's narrow transport client. It consumes
// notification:send (replayed after reconnect) plus, when the launcher asks
// for it, the ephemeral updater:install directive channel, and uses the same
// connection for the RPCs both of those produce.
type NotificationClient struct {
	wsURL string
	token string
	// session forwards the backend's local page-channel credential on
	// every dial, so this connection names a session instead of being
	// trusted for arriving over the WSL localhost relay. Best-effort:
	// see internal/relaysession.
	session           *relaysession.Source
	present           func(notify.Send) error
	handleInstall     func(selfupdate.InstallDirective)
	handleWebviewTrim func(reason string)
	handleKeepAwake   func(mode string)
	handleBrowserHost func(directive webview2host.Directive)
	// channels is the exact subscribe-frame payload, fixed at construction so
	// every reconnect asks for the same set.
	channels []string
	// levelChannels are the subscribed channels whose newest retained
	// frame must be redelivered on every connection. They ride the replay
	// request with a cursor of zero — see HandleKeepAwake.
	levelChannels []string
	logf          func(string, ...any)
	minWait       time.Duration
	maxWait       time.Duration
	// rpcTimeout bounds one RPC exchange (notificationBridgeRPCTimeout in
	// production; tests inject a short one so the disconnected-bridge path is
	// asserted rather than slept through).
	rpcTimeout time.Duration

	connMu sync.RWMutex
	conn   *websocket.Conn
	// connReady is closed after a connection has sent its replay request.
	// Activation may arrive during a cold launcher boot, so callers wait on
	// this edge instead of losing the click while the bridge connects.
	connReady chan struct{}

	writeMu sync.Mutex
	seqMu   sync.Mutex
	lastSeq uint64

	pendingMu sync.Mutex
	pending   map[string]pendingRPC
	nextRPC   atomic.Uint64

	// keepAwake* is the ordered latest-wins mailbox behind
	// handleKeepAwakeDirective. A bare `go handler(mode)` per frame (the
	// webview:trim shape) is wrong for a LEVEL: two frames in quick
	// succession — a replayed boot frame plus a live toggle, or a fast
	// on/off — would race, and the stale mode could apply last, leaving
	// the machine's power state contradicting the setting. One drain
	// goroutine applies modes in arrival order and skips straight to the
	// newest when frames outpace it.
	keepAwakeMu      sync.Mutex
	keepAwakePending *string
	keepAwakeDrain   bool
}

func NewNotificationClient(config NotificationClientConfig) (*NotificationClient, error) {
	if config.WSURL == "" {
		return nil, errors.New("notification bridge websocket URL is required")
	}
	parsed, err := url.Parse(config.WSURL)
	if err != nil {
		return nil, fmt.Errorf("parse notification bridge websocket URL: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("notification bridge websocket URL scheme %q must be ws or wss", parsed.Scheme)
	}
	if config.Token == "" {
		return nil, errors.New("notification bridge token is required")
	}
	if config.Present == nil {
		return nil, errors.New("notification bridge presenter is required")
	}
	minWait := config.MinBackoff
	if minWait <= 0 {
		minWait = 100 * time.Millisecond
	}
	maxWait := config.MaxBackoff
	if maxWait <= 0 {
		maxWait = 5 * time.Second
	}
	if maxWait < minWait {
		maxWait = minWait
	}
	logf := config.Logf
	if logf == nil {
		logf = log.Printf
	}
	channels := []string{notify.SendChannel}
	if config.HandleUpdateInstall != nil {
		channels = append(channels, selfupdate.ChannelInstall)
	}
	if config.HandleWebviewTrim != nil {
		channels = append(channels, string(eventchan.WebviewTrim))
	}
	if config.HandleBrowserHost != nil {
		channels = append(channels, string(eventchan.BrowserHost))
	}
	var levelChannels []string
	if config.HandleKeepAwake != nil {
		channels = append(channels, string(eventchan.PowerKeepAwake))
		levelChannels = append(levelChannels, string(eventchan.PowerKeepAwake))
	}
	return &NotificationClient{
		wsURL:             parsed.String(),
		token:             config.Token,
		session:           newSessionCredentialSource(parsed.String(), config.Token),
		present:           config.Present,
		handleInstall:     config.HandleUpdateInstall,
		handleWebviewTrim: config.HandleWebviewTrim,
		handleKeepAwake:   config.HandleKeepAwake,
		handleBrowserHost: config.HandleBrowserHost,
		channels:          channels,
		levelChannels:     levelChannels,
		logf:              logf,
		minWait:           minWait,
		maxWait:           maxWait,
		rpcTimeout:        notificationBridgeRPCTimeout,
		pending:           make(map[string]pendingRPC),
		connReady:         make(chan struct{}),
	}, nil
}

// newSessionCredentialSource points a relaysession.Source at the backend
// this client dials.
//
// A WebSocket URL that will not map onto a bootstrap URL yields an inert
// source rather than an error: forwarding the credential is an
// improvement in attribution, and refusing to construct the notification
// client over it would trade a working launcher for a better-labelled
// one. The dial that follows carries the launch token alone, which is
// what it carried before forwarding existed.
func newSessionCredentialSource(wsURL, token string) *relaysession.Source {
	bootstrap, err := relaysession.BootstrapURL(wsURL)
	if err != nil {
		log.Printf("wsllauncher: no session credential to forward: %v", err)
	}
	return relaysession.New(bootstrap, token, nil)
}

// Run reconnects until ctx is cancelled. Connection and presentation errors
// are diagnostic state, not launcher-fatal errors.
func (c *NotificationClient) Run(ctx context.Context) {
	wait := c.minWait
	for ctx.Err() == nil {
		attemptStarted := time.Now()
		connected, err := c.runConnection(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.logf("notifications: launcher bridge disconnected: %v", err)
		}
		if connected && time.Since(attemptStarted) >= 5*time.Second {
			wait = c.minWait
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
		if wait < c.maxWait {
			wait *= 2
			if wait > c.maxWait {
				wait = c.maxWait
			}
		}
	}
}

func (c *NotificationClient) runConnection(ctx context.Context) (bool, error) {
	parsed, err := url.Parse(c.wsURL)
	if err != nil {
		return false, err
	}
	query := parsed.Query()
	query.Set("token", c.token)
	parsed.RawQuery = query.Encode()

	// The session credential rides a HEADER, never the URL: a Go dial can
	// set one, and a credential in a URL lands in every log that records
	// them. Absent when the backend has none to give, which leaves this
	// connection exactly as it was before forwarding existed.
	var opts *websocket.DialOptions
	if credential := c.session.Credential(ctx); credential != "" {
		opts = &websocket.DialOptions{HTTPHeader: http.Header{
			relaysession.Header: []string{credential},
		}}
	}
	conn, _, err := websocket.Dial(ctx, parsed.String(), opts)
	if err != nil {
		// A refused dial is the one signal that a forwarded credential has
		// gone stale — the backend restarted, or the session was revoked.
		// Mark it so the next attempt in the ladder fetches a live one
		// rather than replaying the dead one until the launcher restarts.
		c.session.Stale()
		return false, fmt.Errorf("connect to notification bridge: %s", redactNotificationBridgeError(err, c.token))
	}
	conn.SetReadLimit(notificationBridgeReadLimit)
	defer func() {
		c.clearConnection(conn, ErrNotificationBridgeDisconnected)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()
	if err := c.writeJSON(ctx, conn, notificationClientFrame{
		Type:     "subscribe",
		Channels: c.channels,
	}); err != nil {
		return true, fmt.Errorf("subscribe to notification channel: %w", err)
	}

	// notification:send is the only channel that carries a MOVING replay
	// cursor: its frames are history, and the launcher must not miss or
	// repeat one. updater:install and webview:trim are ephemeral on the
	// server (never retained) and only actionable in the moment they were
	// emitted — a channel absent from lastSeqByChannel gets no replay,
	// which is exactly right for them.
	//
	// power:keepawake is the third shape: a retained LEVEL. It rides the
	// same frame with a cursor pinned at ZERO, which on its latest-only
	// ring means "send me the newest frame, whenever it was emitted" —
	// that is the whole convergence mechanism after a reconnect. Zero, not
	// a tracked cursor: a backend restart re-seeds every channel's seq
	// from 1, and a remembered cursor above the new head would come back
	// as a gap marker instead of the state.
	c.seqMu.Lock()
	lastSeq := c.lastSeq
	c.seqMu.Unlock()
	cursors := make(map[string]uint64, 1+len(c.levelChannels))
	cursors[notify.SendChannel] = lastSeq
	for _, channel := range c.levelChannels {
		cursors[channel] = 0
	}
	if err := c.writeJSON(ctx, conn, notificationClientFrame{
		Type:             "replay",
		LastSeqByChannel: cursors,
	}); err != nil {
		return true, fmt.Errorf("request replay: %w", err)
	}
	c.setConnection(conn)
	c.logf("notifications: launcher bridge connected")
	replayPending := true
	pendingReplay := make([]notificationEvent, 0, 4)

	for {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			return true, err
		}
		var frame notificationServerFrame
		if err := json.Unmarshal(payload, &frame); err != nil {
			c.logf("notifications: ignore malformed bridge frame: %v", err)
			continue
		}
		switch frame.Type {
		case "rpc":
			c.resolveRPC(frame)
		case "replay":
			if err := c.handleReplayEvents(pendingReplay); err != nil {
				return true, err
			}
			pendingReplay = pendingReplay[:0]
			replayPending = false
		case "event":
			if replayPending && frame.Channel == notify.SendChannel {
				pendingReplay = append(pendingReplay, frame.notificationEvent)
				continue
			}
			if err := c.handleEvent(frame.notificationEvent); err != nil {
				return true, err
			}
		case "batch":
			for _, event := range frame.Events {
				if replayPending && event.Channel == notify.SendChannel {
					pendingReplay = append(pendingReplay, event)
					continue
				}
				if err := c.handleEvent(event); err != nil {
					return true, err
				}
			}
		}
	}
}

func (c *NotificationClient) handleReplayEvents(events []notificationEvent) error {
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Seq < events[j].Seq
	})
	for _, event := range events {
		if err := c.handleEvent(event); err != nil {
			return err
		}
	}
	return nil
}

func redactNotificationBridgeError(err error, token string) string {
	if err == nil {
		return "unknown error"
	}
	message := err.Error()
	if token == "" {
		return message
	}
	message = strings.ReplaceAll(message, token, "[redacted]")
	message = strings.ReplaceAll(message, url.QueryEscape(token), "[redacted]")
	return message
}

func (c *NotificationClient) handleEvent(event notificationEvent) error {
	if event.Channel == selfupdate.ChannelInstall {
		c.handleInstallDirective(event.Data)
		return nil
	}
	if event.Channel == string(eventchan.WebviewTrim) {
		c.handleWebviewTrimDirective(event.Data)
		return nil
	}
	if event.Channel == string(eventchan.BrowserHost) {
		c.handleBrowserHostDirective(event.Data)
		return nil
	}
	if event.Channel == string(eventchan.PowerKeepAwake) {
		if event.Gap {
			// Cannot happen with a zero cursor on a latest-only ring, but
			// a gap marker carries `null` data and no mode — acting on it
			// would mean guessing at the machine's power state.
			c.logf("keep awake: ignore replay gap marker at sequence %d", event.Seq)
			return nil
		}
		c.handleKeepAwakeDirective(event.Data)
		return nil
	}
	if event.Channel != notify.SendChannel {
		return nil
	}
	c.seqMu.Lock()
	if event.Seq <= c.lastSeq {
		c.seqMu.Unlock()
		return nil
	}
	if c.lastSeq != 0 && event.Seq > c.lastSeq+1 {
		lastSeq := c.lastSeq
		c.seqMu.Unlock()
		return fmt.Errorf("notification sequence gap: received %d after %d", event.Seq, lastSeq)
	}
	c.lastSeq = event.Seq
	c.seqMu.Unlock()
	if event.Gap {
		c.logf("notifications: launcher bridge replay gap at sequence %d", event.Seq)
		return nil
	}

	var notification notify.Send
	if err := json.Unmarshal(event.Data, &notification); err != nil {
		c.logf("notifications: ignore malformed notification payload: %v", err)
		return nil
	}
	// One admission check, shared with the backend that published this
	// (notify.ValidateSend), rather than three restatements of it here. The
	// re-check is not redundant: this crossed a process boundary, and the
	// launcher is the side that would present whatever it decoded. It is
	// also what admits a retraction, whose contract is deliberately the
	// opposite of a presentation's — an id and a kind, and no content.
	if err := notify.ValidateSend(notification); err != nil {
		c.logf("notifications: ignore invalid notification: %v", err)
		return nil
	}
	if err := c.present(notification); err != nil {
		c.logf("notifications: present bridged notification %s: %v", notification.ID, err)
	}
	return nil
}

// handleInstallDirective decodes and validates one updater:install frame and
// hands it to the launcher. A directive that fails validation is dropped with
// a log line and never reaches the handler: it names a file the launcher would
// otherwise resolve on disk, so validation is the trust boundary, not a
// formatting nicety. A malformed frame is never connection-fatal — dropping
// one directive costs the user a retry, while returning an error would tear
// down the notification bridge with it.
func (c *NotificationClient) handleInstallDirective(data json.RawMessage) {
	if c.handleInstall == nil {
		// Unsubscribed channel; the server should never push it here.
		c.logf("updater: ignore install directive on an unsubscribed connection")
		return
	}
	var directive selfupdate.InstallDirective
	if err := json.Unmarshal(data, &directive); err != nil {
		c.logf("updater: ignore malformed install directive: %v", err)
		return
	}
	if err := directive.Validate(); err != nil {
		c.logf("updater: ignore invalid install directive: %v", err)
		return
	}
	// Off the read loop: the handler answers by posting an RPC over this same
	// connection and can only see the response if the read loop keeps running.
	go c.handleInstall(directive)
}

// handleWebviewTrimDirective decodes one webview:trim frame and hands its
// reason to the launcher. A malformed frame is logged and dropped, never
// connection-fatal — losing one trim costs nothing, the backend re-emits on
// the next idle report. Off the read loop for the same reason as the install
// directive: the handler runs a DevTools round-trip and must not stall
// notification delivery behind it.
func (c *NotificationClient) handleWebviewTrimDirective(data json.RawMessage) {
	if c.handleWebviewTrim == nil {
		c.logf("webview trim: ignore directive on an unsubscribed connection")
		return
	}
	var directive struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &directive); err != nil {
		c.logf("webview trim: ignore malformed directive: %v", err)
		return
	}
	go c.handleWebviewTrim(directive.Reason)
}

// handleBrowserHostDirective decodes and validates one browser:host frame
// and hands it to the launcher's pane host. A frame that fails to decode
// or fails Validate is logged and dropped, never connection-fatal: the
// directive commands real browser windows, so a garbled one must not be
// guessed at, and losing one costs a redraw the backend re-derives rather
// than the whole notification bridge.
//
// Off the read loop like its siblings, and here that is load-bearing
// rather than merely polite: the handler blocks on the launcher's UI
// thread and, on the first directive, on a cold WebView2 environment
// create that takes seconds. Running it inline would stall notification
// delivery and the report RPC the handler itself posts back over this
// same connection.
func (c *NotificationClient) handleBrowserHostDirective(data json.RawMessage) {
	if c.handleBrowserHost == nil {
		c.logf("browser host: ignore directive on an unsubscribed connection")
		return
	}
	var directive webview2host.Directive
	if err := json.Unmarshal(data, &directive); err != nil {
		c.logf("browser host: ignore malformed directive: %v", err)
		return
	}
	if err := directive.Validate(); err != nil {
		c.logf("browser host: ignore invalid directive: %v", err)
		return
	}
	go c.handleBrowserHost(directive)
}

// handleKeepAwakeDirective decodes one power:keepawake frame and hands its
// mode to the launcher. A malformed frame is logged and dropped rather
// than being connection-fatal, and a frame with no recognizable mode is
// dropped rather than defaulted: this directive commands the machine's
// power state, and guessing would either pin a laptop awake on a garbled
// frame or release an inhibit the user asked for.
//
// Off the read loop like its two siblings — the handler hands work to the
// execution-state holder thread and must not stall notification delivery.
func (c *NotificationClient) handleKeepAwakeDirective(data json.RawMessage) {
	if c.handleKeepAwake == nil {
		c.logf("keep awake: ignore directive on an unsubscribed connection")
		return
	}
	var directive struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &directive); err != nil {
		c.logf("keep awake: ignore malformed directive: %v", err)
		return
	}
	if directive.Mode == "" {
		c.logf("keep awake: ignore directive with no mode")
		return
	}
	c.keepAwakeMu.Lock()
	c.keepAwakePending = &directive.Mode
	startDrain := !c.keepAwakeDrain
	c.keepAwakeDrain = true
	c.keepAwakeMu.Unlock()
	if startDrain {
		go c.drainKeepAwake()
	}
}

// drainKeepAwake applies pending keep-awake modes until the mailbox is
// empty, then exits; handleKeepAwakeDirective restarts it on the next
// frame. Only the newest pending mode is ever applied — intermediate
// modes that arrived while the handler ran are convergence noise, not
// history.
func (c *NotificationClient) drainKeepAwake() {
	for {
		c.keepAwakeMu.Lock()
		mode := c.keepAwakePending
		c.keepAwakePending = nil
		if mode == nil {
			c.keepAwakeDrain = false
			c.keepAwakeMu.Unlock()
			return
		}
		c.keepAwakeMu.Unlock()
		c.handleKeepAwake(*mode)
	}
}

func (c *NotificationClient) writeJSON(ctx context.Context, conn *websocket.Conn, frame notificationClientFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.Write(ctx, websocket.MessageText, payload)
}

func (c *NotificationClient) setConnection(conn *websocket.Conn) {
	c.connMu.Lock()
	c.conn = conn
	close(c.connReady)
	c.connMu.Unlock()
}

func (c *NotificationClient) clearConnection(conn *websocket.Conn, err error) {
	c.connMu.Lock()
	cleared := false
	if c.conn == conn {
		c.conn = nil
		c.connReady = make(chan struct{})
		cleared = true
	}
	c.connMu.Unlock()
	if cleared {
		c.failPending(err)
	}
}

func (c *NotificationClient) waitForConnection(ctx context.Context) (*websocket.Conn, error) {
	for {
		c.connMu.RLock()
		conn := c.conn
		ready := c.connReady
		c.connMu.RUnlock()
		if conn != nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for notification bridge connection: %w", ctx.Err())
		case <-ready:
		}
	}
}

type notificationClientFrame struct {
	Type             string            `json:"type"`
	ID               string            `json:"id,omitempty"`
	Method           string            `json:"method,omitempty"`
	Params           []json.RawMessage `json:"params,omitempty"`
	LastSeqByChannel map[string]uint64 `json:"lastSeqByChannel,omitempty"`
	Channels         []string          `json:"channels,omitempty"`
}

type notificationFrameError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type notificationEvent struct {
	Channel string          `json:"channel"`
	Seq     uint64          `json:"seq"`
	Data    json.RawMessage `json:"data"`
	Gap     bool            `json:"gap,omitempty"`
}

type notificationServerFrame struct {
	notificationEvent
	Type   string                  `json:"type"`
	ID     string                  `json:"id,omitempty"`
	Error  *notificationFrameError `json:"error,omitempty"`
	Events []notificationEvent     `json:"events,omitempty"`
}
