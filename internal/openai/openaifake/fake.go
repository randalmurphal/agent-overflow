// Package openaifake answers OpenAI's live transcription in process, for
// tests and isolated boots, so the real client runs end to end without a
// network. Nothing listens on a socket: each handshake is served over an
// in-memory pipe.
//
// The fake checks the session the client configures the way OpenAI does
// for the fields this client sends, then answers deterministically: every
// appended piece of audio extends the partial text of the turn in progress
// ("Hearing", then a dot per further piece), and a commit transcribes the
// turn as "Heard <n> bytes of live audio" under its own item id and starts
// an empty one, as OpenAI's buffer does. A commit with under 100 ms of
// audio is an error, as OpenAI's is.
//
// Two keys script a misbehaving OpenAI: RefusedKey is refused at the
// handshake and StallsKey stops reading once the session is configured. A
// test can also Hold a key of its own, so sessions opened with it withhold
// every transcript until the test releases them.
package openaifake

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"agent-overflow/internal/openai"
)

// The keys that script a misbehaving OpenAI.
const (
	RefusedKey = "sk-fake-refused-key"
	StallsKey  = "sk-fake-stalls"
)

// held maps a held key to the channel whose closing releases its
// sessions' transcripts.
var held sync.Map

// Hold makes sessions opened with key withhold every transcript until
// release runs. Use a key no other test uses.
func Hold(key string) (release func()) {
	released := make(chan struct{})
	held.Store(key, released)
	var once sync.Once
	return func() {
		once.Do(func() {
			held.Delete(key)
			close(released)
		})
	}
}

// stallFor is how long a StallsKey session stops reading.
const stallFor = 30 * time.Second

// minCommitBytes is 100 ms of 24 kHz 16-bit audio, the least OpenAI
// commits.
const minCommitBytes = openai.LiveSampleRate * 2 / 10

// ErrNotFaked refuses any request the fake does not answer.
var ErrNotFaked = errors.New("openaifake: no fake answers this request")

// Transport answers the live transcription handshake and refuses
// everything else.
type Transport struct{}

// IsLiveTranscription reports whether req is the live transcription
// handshake, as coder/websocket sends it (wss rewritten to https).
func IsLiveTranscription(req *http.Request) bool {
	return req.Method == http.MethodGet && req.URL.Scheme == "https" && req.URL.Host == "api.openai.com" &&
		req.URL.Path == "/v1/realtime" && req.URL.Query().Get("intent") == "transcription"
}

func (Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !IsLiveTranscription(req) {
		return nil, ErrNotFaked
	}
	return ServeLiveTranscription(req)
}

// ServeLiveTranscription answers one handshake over an in-memory pipe and
// runs the session on the server end until either side closes it.
func ServeLiveTranscription(req *http.Request) (*http.Response, error) {
	client, server := net.Pipe()
	go serve(server)
	if err := req.Write(client); err != nil {
		return nil, errors.Join(err, client.Close())
	}
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, errors.Join(err, client.Close())
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		response.Body = pipeBody{Reader: reader, Conn: client}
	}
	return response, nil
}

// pipeBody is the upgraded connection coder/websocket expects as the
// body of a 101: reads first drain what ReadResponse buffered.
type pipeBody struct {
	io.Reader
	net.Conn
}

func (b pipeBody) Read(p []byte) (int, error) { return b.Reader.Read(p) }

// serve runs the fake's end of one pipe. It has no caller to report to:
// a failure here shows up at the client as a dead connection.
func serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	buffered := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	req, err := http.ReadRequest(buffered.Reader)
	if err != nil {
		return
	}
	writer := &hijackable{conn: conn, buffered: buffered, header: http.Header{}}
	if req.Header.Get("Authorization") == "Bearer "+RefusedKey {
		http.Error(writer, `{"error":{"message":"Incorrect API key provided: refused."}}`, http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(writer, req, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	key := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	var release chan struct{}
	if found, ok := held.Load(key); ok {
		release = found.(chan struct{})
	}
	(&session{ws: ws, stalls: key == StallsKey, release: release}).run(context.Background())
}

// hijackable is the ResponseWriter websocket.Accept needs: it writes the
// response head straight to the pipe and hands the pipe over.
type hijackable struct {
	conn     net.Conn
	buffered *bufio.ReadWriter
	header   http.Header
	wrote    bool
}

func (w *hijackable) Header() http.Header { return w.header }

func (w *hijackable) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	// The pipe's reader sees a failed write as a broken response.
	_, _ = fmt.Fprintf(w.buffered, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	_ = w.header.Write(w.buffered)
	_, _ = fmt.Fprint(w.buffered, "\r\n")
	_ = w.buffered.Flush()
}

func (w *hijackable) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	n, err := w.buffered.Write(p)
	if err == nil {
		err = w.buffered.Flush()
	}
	return n, err
}

func (w *hijackable) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, w.buffered, nil
}

type session struct {
	ws     *websocket.Conn
	stalls bool
	// release, when set, withholds transcripts until it is closed.
	release chan struct{}
	// turn counts commits; the turn in progress is item_<turn+1>.
	turn  int
	heard int
	parts int
}

func (s *session) item() string { return fmt.Sprintf("item_%d", s.turn+1) }

func (s *session) run(ctx context.Context) {
	for {
		_, payload, err := s.ws.Read(ctx)
		if err != nil {
			return
		}
		var event struct {
			Type    string          `json:"type"`
			Audio   string          `json:"audio"`
			Session json.RawMessage `json:"session"`
		}
		if json.Unmarshal(payload, &event) != nil {
			s.fail(ctx, "invalid_json", "The event is not JSON.")
			continue
		}
		switch event.Type {
		case "session.update":
			if problem := checkSession(event.Session); problem != "" {
				s.fail(ctx, "invalid_value", problem)
				continue
			}
			s.send(ctx, map[string]any{"type": "session.updated", "session": json.RawMessage(event.Session)})
			if s.stalls {
				// Stop reading, so the client's writes back up; give up
				// after a while so the stalled session does not outlive
				// its test.
				<-time.After(stallFor)
				return
			}
		case "input_audio_buffer.append":
			audio, err := base64.StdEncoding.DecodeString(event.Audio)
			if err != nil || len(audio) == 0 {
				s.fail(ctx, "invalid_value", "The audio is not base64 PCM.")
				continue
			}
			s.heard += len(audio)
			delta := "."
			if s.parts == 0 {
				delta = "Hearing"
			}
			s.parts++
			s.send(ctx, map[string]any{"type": "conversation.item.input_audio_transcription.delta", "item_id": s.item(), "delta": delta})
		case "input_audio_buffer.commit":
			if s.heard < minCommitBytes {
				s.fail(ctx, "input_audio_buffer_commit_empty", "Error committing input audio buffer: buffer too small.")
				continue
			}
			item, heard := s.item(), s.heard
			s.turn, s.heard, s.parts = s.turn+1, 0, 0
			s.send(ctx, map[string]any{"type": "input_audio_buffer.committed", "item_id": item})
			completed := map[string]any{
				"type":       "conversation.item.input_audio_transcription.completed",
				"item_id":    item,
				"transcript": fmt.Sprintf("Heard %d bytes of live audio", heard),
			}
			if s.release != nil {
				go func() {
					<-s.release
					s.send(ctx, completed)
				}()
			} else {
				s.send(ctx, completed)
			}
		default:
			s.fail(ctx, "invalid_event", "Unknown event type "+event.Type+".")
		}
	}
}

// checkSession reports what OpenAI would refuse in the session this
// client configures, or "".
func checkSession(raw json.RawMessage) string {
	var session struct {
		Type  string `json:"type"`
		Audio struct {
			Input struct {
				Format struct {
					Type string `json:"type"`
					Rate int    `json:"rate"`
				} `json:"format"`
				Transcription struct {
					Model string `json:"model"`
				} `json:"transcription"`
				TurnDetection json.RawMessage `json:"turn_detection"`
			} `json:"input"`
		} `json:"audio"`
	}
	if json.Unmarshal(raw, &session) != nil {
		return "The session is not an object."
	}
	input := session.Audio.Input
	switch {
	case session.Type != "transcription":
		return "Only a transcription session is supported here."
	case input.Format.Type != "audio/pcm" || input.Format.Rate != openai.LiveSampleRate:
		return "The audio format must be 24 kHz PCM."
	case input.Transcription.Model != openai.LiveTranscriptionModel:
		return "Unsupported transcription model."
	case string(input.TurnDetection) != "null":
		return "gpt-live-transcribe requires turn detection to be null."
	}
	return ""
}

func (s *session) send(ctx context.Context, event map[string]any) {
	payload, err := json.Marshal(event)
	if err == nil {
		_ = s.ws.Write(ctx, websocket.MessageText, payload)
	}
}

func (s *session) fail(ctx context.Context, code, message string) {
	s.send(ctx, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "code": code, "message": message}})
}
