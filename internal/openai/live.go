package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"agent-overflow/internal/secret"
)

// Live transcription: a Realtime API session in transcription mode with
// turn detection off. The caller streams 16-bit PCM as the user speaks,
// reads the text OpenAI has heard so far, and on Finish commits the audio
// as one turn and gets its transcript.

// LiveTranscriptionURL opens a transcription session. The intent query,
// with no model, is what makes it transcription-only.
const LiveTranscriptionURL = "wss://" + apiHost + "/v1/realtime?intent=transcription"

// LiveTranscriptionModel is OpenAI's low-latency streaming transcription
// model. It requires turn detection off, so the client ends the turn.
const LiveTranscriptionModel = "gpt-live-transcribe"

// LiveSampleRate is the rate of the PCM a session takes: 16-bit signed
// little-endian mono.
const LiveSampleRate = 24000

// LiveDelays are the model's latency settings, earliest partial text
// first; higher is more accurate.
var LiveDelays = []string{"minimal", "low", "medium", "high", "xhigh"}

// maxEventBytes bounds one event read from OpenAI. Transcription events
// carry text, not audio.
const maxEventBytes = 1 << 20

// LiveTranscriptionOptions are the user's settings for the model. Empty
// fields leave OpenAI's defaults.
type LiveTranscriptionOptions struct {
	Delay     string
	Languages []string
	Keywords  []string
	Prompt    string
}

// ErrFinished refuses audio or a second Finish once a session's Finish
// has begun.
var ErrFinished = errors.New("the live recording was already finished")

// errRedirected refuses a redirected handshake: following it would send
// the key to a URL this package did not choose.
var errRedirected = errors.New("OpenAI live transcription redirected the connection")

// LiveTranscription is one open session.
type LiveTranscription struct {
	conn *websocket.Conn
	// gate admits one write at a time, and gives up when its caller's
	// context ends.
	gate chan struct{}
	stop context.CancelFunc
	done chan struct{}

	mu sync.Mutex
	// finishing is set, holding the gate, by the Finish that writes the
	// commit; no audio is written after it.
	finishing bool
	// items lists transcript items in the order OpenAI first named them.
	items   []string
	partial map[string]string
	final   map[string]string
	// committed is the item the Finish commit became.
	committed string
	// err ends the session: an error event, a failed item, or the socket
	// closing.
	err error
	// changed is closed and replaced on every state change.
	changed chan struct{}
}

// DialLiveTranscription opens a session and waits until OpenAI accepts its
// configuration. The key is read only to build the handshake header.
func DialLiveTranscription(ctx context.Context, client *http.Client, key *secret.Bytes, options LiveTranscriptionOptions) (*LiveTranscription, error) {
	if client == nil {
		client = http.DefaultClient
	}
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirected }
	conn, response, err := websocket.Dial(ctx, LiveTranscriptionURL, &websocket.DialOptions{
		HTTPClient: &noRedirects,
		HTTPHeader: authorization(key),
	})
	if err != nil {
		if response != nil {
			switch response.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				return nil, ErrKeyRefused
			case http.StatusTooManyRequests:
				return nil, ErrRateLimited
			}
			// The reason phrase is OpenAI's text; say the status in Go's.
			return nil, fmt.Errorf("connect to OpenAI live transcription: %d %s", response.StatusCode, http.StatusText(response.StatusCode))
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("connect to OpenAI live transcription: %s", scrub(err.Error()))
	}
	conn.SetReadLimit(maxEventBytes)
	session, stop := context.WithCancel(context.Background())
	s := &LiveTranscription{
		conn:    conn,
		gate:    make(chan struct{}, 1),
		stop:    stop,
		done:    make(chan struct{}),
		partial: map[string]string{},
		final:   map[string]string{},
		changed: make(chan struct{}),
	}
	configured := make(chan error, 1)
	go s.read(session, configured)
	if err := s.send(ctx, sessionUpdate(options), nil); err != nil {
		s.Close()
		return nil, err
	}
	select {
	case err := <-configured:
		if err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	case <-ctx.Done():
		s.Close()
		return nil, ctx.Err()
	}
}

func sessionUpdate(options LiveTranscriptionOptions) map[string]any {
	transcription := map[string]any{"model": LiveTranscriptionModel}
	if options.Delay != "" {
		transcription["delay"] = options.Delay
	}
	if len(options.Languages) > 0 {
		transcription["languages"] = options.Languages
	}
	if len(options.Keywords) > 0 {
		transcription["keywords"] = options.Keywords
	}
	if options.Prompt != "" {
		transcription["prompt"] = options.Prompt
	}
	return map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "transcription",
			"audio": map[string]any{
				"input": map[string]any{
					"format":         map[string]any{"type": "audio/pcm", "rate": LiveSampleRate},
					"transcription":  transcription,
					"turn_detection": nil,
				},
			},
		},
	}
}

// Append streams more of the recording. Once Finish has begun it refuses
// with ErrFinished: that audio would belong to no transcript.
func (s *LiveTranscription) Append(ctx context.Context, pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	return s.send(ctx, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(pcm),
	}, func() error {
		if s.finishing {
			return ErrFinished
		}
		return nil
	})
}

// Heard is the text OpenAI has transcribed so far, partial text included.
func (s *LiveTranscription) Heard() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	pieces := make([]string, 0, len(s.items))
	for _, item := range s.items {
		text, ok := s.final[item]
		if !ok {
			text = s.partial[item]
		}
		if text = strings.TrimSpace(text); text != "" {
			pieces = append(pieces, text)
		}
	}
	return strings.Join(pieces, " ")
}

// Finish commits the recording as one turn, waits for its transcript and
// closes the session. Only the first Finish to be admitted commits and
// owns the session from then on; any other refuses with ErrFinished, or
// with its own context's error, and leaves the session to its owner.
func (s *LiveTranscription) Finish(ctx context.Context) (string, error) {
	owner := false
	err := s.send(ctx, map[string]any{"type": "input_audio_buffer.commit"}, func() error {
		if s.finishing {
			return ErrFinished
		}
		s.finishing = true
		owner = true
		return nil
	})
	if !owner {
		return "", err
	}
	defer s.Close()
	if err != nil {
		return "", err
	}
	for {
		s.mu.Lock()
		if s.committed != "" {
			if transcript, ok := s.final[s.committed]; ok {
				s.mu.Unlock()
				return strings.TrimSpace(transcript), nil
			}
		}
		err, changed := s.err, s.changed
		s.mu.Unlock()
		if err != nil {
			return "", err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// Close ends the session at once, failing a write still in progress.
// Closing a closed session does nothing.
func (s *LiveTranscription) Close() {
	s.stop()
	_ = s.conn.CloseNow()
	<-s.done
}

func (s *LiveTranscription) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// send writes one event. admit, when set, runs holding the gate and the
// state lock, so it orders the write against Finish's commit.
func (s *LiveTranscription) send(ctx context.Context, event map[string]any, admit func() error) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.gate }()
	s.mu.Lock()
	failed := s.err
	if failed == nil && admit != nil {
		failed = admit()
	}
	s.mu.Unlock()
	if failed != nil {
		return failed
	}
	if err := s.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		// Cancelling a blocked write closes the socket, which the reader
		// may record first as a generic end; the caller's cancellation is
		// the cause.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if failed := s.failure(); failed != nil {
			return failed
		}
		return fmt.Errorf("send to OpenAI live transcription: %w", err)
	}
	return nil
}

// serverEvent is the part of an OpenAI event this client reads.
type serverEvent struct {
	Type       string `json:"type"`
	ItemID     string `json:"item_id"`
	Delta      string `json:"delta"`
	Transcript string `json:"transcript"`
	Error      *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *LiveTranscription) read(ctx context.Context, configured chan<- error) {
	defer close(s.done)
	settled := false
	settle := func(err error) {
		if !settled {
			settled = true
			configured <- err
		}
	}
	for {
		_, payload, err := s.conn.Read(ctx)
		if err != nil {
			ended := errors.New("OpenAI ended the live transcription")
			if ctx.Err() != nil {
				ended = errors.New("the live transcription was closed")
			}
			s.fail(ended)
			settle(ended)
			return
		}
		var event serverEvent
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		switch event.Type {
		case "session.updated":
			settle(nil)
		case "error", "conversation.item.input_audio_transcription.failed":
			err := errors.New("OpenAI could not transcribe the recording")
			if event.Error != nil && event.Error.Message != "" {
				err = fmt.Errorf("OpenAI could not transcribe the recording: %s", scrub(event.Error.Message))
			}
			s.fail(err)
			settle(err)
		case "conversation.item.input_audio_transcription.delta":
			s.update(func() {
				s.note(event.ItemID)
				s.partial[event.ItemID] += event.Delta
			})
		case "conversation.item.input_audio_transcription.completed":
			s.update(func() {
				s.note(event.ItemID)
				s.final[event.ItemID] = event.Transcript
			})
		case "input_audio_buffer.committed":
			s.update(func() { s.committed = event.ItemID })
		}
	}
}

// note records an item the first time OpenAI names it. Held under mu.
func (s *LiveTranscription) note(item string) {
	if _, seen := s.partial[item]; seen {
		return
	}
	if _, seen := s.final[item]; seen {
		return
	}
	s.items = append(s.items, item)
	s.partial[item] = ""
}

func (s *LiveTranscription) fail(err error) {
	s.update(func() {
		if s.err == nil {
			s.err = err
		}
	})
}

func (s *LiveTranscription) update(change func()) {
	s.mu.Lock()
	change()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}
