package wsllauncher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/notify"

	"github.com/coder/websocket"
)

var ErrNotificationBridgeDisconnected = errors.New("notification bridge disconnected")

const notificationActivationRPCTimeout = 5 * time.Second
const notificationBridgeReadLimit = 1024 * 1024

type NotificationClientConfig struct {
	WSURL      string
	Token      string
	Present    func(notify.Send) error
	Logf       func(string, ...any)
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

// NotificationClient is the launcher's narrow transport client. It consumes
// only notification:send, replays that channel after reconnect, and uses the
// same connection for activation RPCs.
type NotificationClient struct {
	wsURL   string
	token   string
	present func(notify.Send) error
	logf    func(string, ...any)
	minWait time.Duration
	maxWait time.Duration

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
	pending   map[string]chan notificationRPCResult
	nextRPC   atomic.Uint64
}

type notificationRPCResult struct {
	err error
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
	return &NotificationClient{
		wsURL:     parsed.String(),
		token:     config.Token,
		present:   config.Present,
		logf:      logf,
		minWait:   minWait,
		maxWait:   maxWait,
		pending:   make(map[string]chan notificationRPCResult),
		connReady: make(chan struct{}),
	}, nil
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

	conn, _, err := websocket.Dial(ctx, parsed.String(), nil)
	if err != nil {
		return false, fmt.Errorf("connect to notification bridge: %s", redactNotificationBridgeError(err, c.token))
	}
	conn.SetReadLimit(notificationBridgeReadLimit)
	defer func() {
		c.clearConnection(conn, ErrNotificationBridgeDisconnected)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()
	if err := c.writeJSON(ctx, conn, notificationClientFrame{
		Type:     "subscribe",
		Channels: []string{notify.SendChannel},
	}); err != nil {
		return true, fmt.Errorf("subscribe to notification channel: %w", err)
	}

	c.seqMu.Lock()
	lastSeq := c.lastSeq
	c.seqMu.Unlock()
	if err := c.writeJSON(ctx, conn, notificationClientFrame{
		Type:             "replay",
		LastSeqByChannel: map[string]uint64{notify.SendChannel: lastSeq},
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

// Activate posts the validated target back to the backend's local-only
// NotificationActivated RPC and waits for its response.
func (c *NotificationClient) Activate(ctx context.Context, target notify.Target) error {
	if err := notify.ValidateTarget(target); err != nil {
		return err
	}
	conn, err := c.waitForConnection(ctx)
	if err != nil {
		return err
	}

	id := fmt.Sprintf("notification-%d", c.nextRPC.Add(1))
	result := make(chan notificationRPCResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = result
	c.pendingMu.Unlock()

	params, err := json.Marshal(target)
	if err != nil {
		c.removePending(id)
		return fmt.Errorf("encode notification activation target: %w", err)
	}
	rpcCtx, cancel := context.WithTimeout(ctx, notificationActivationRPCTimeout)
	defer cancel()
	if err := c.writeJSON(rpcCtx, conn, notificationClientFrame{
		Type:   "rpc",
		ID:     id,
		Method: "NotificationActivated",
		Params: []json.RawMessage{params},
	}); err != nil {
		c.removePending(id)
		return fmt.Errorf("%w: write notification activation RPC: %v", ErrNotificationBridgeDisconnected, err)
	}

	select {
	case <-rpcCtx.Done():
		c.removePending(id)
		return fmt.Errorf("notification activation RPC: %w", rpcCtx.Err())
	case response := <-result:
		return response.err
	}
}

func (c *NotificationClient) handleEvent(event notificationEvent) error {
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
	if notification.ID == "" || notification.Title == "" {
		c.logf("notifications: ignore notification with missing id or title")
		return nil
	}
	if len(notification.Title) > notify.MaxTitleBytes || len(notification.Body) > notify.MaxBodyBytes {
		c.logf("notifications: ignore oversized notification %s", notification.ID)
		return nil
	}
	if err := notify.ValidateTarget(notification.Target); err != nil {
		c.logf("notifications: ignore invalid notification target: %v", err)
		return nil
	}
	if err := c.present(notification); err != nil {
		c.logf("notifications: present bridged notification %s: %v", notification.ID, err)
	}
	return nil
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

func (c *NotificationClient) resolveRPC(frame notificationServerFrame) {
	c.pendingMu.Lock()
	result := c.pending[frame.ID]
	delete(c.pending, frame.ID)
	c.pendingMu.Unlock()
	if result == nil {
		return
	}
	if frame.Error != nil {
		result <- notificationRPCResult{err: fmt.Errorf("notification activation RPC %s: %s", frame.Error.Code, frame.Error.Message)}
		return
	}
	result <- notificationRPCResult{}
}

func (c *NotificationClient) removePending(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *NotificationClient) failPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan notificationRPCResult)
	c.pendingMu.Unlock()
	for _, result := range pending {
		result <- notificationRPCResult{err: err}
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
