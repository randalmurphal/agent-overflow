package openaifake

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"agent-overflow/internal/openai"
)

// Speech: the fake checks the session.update and response.create the
// client sends against the shapes openai.Speak uses, refusing anything else
// with an error event, and reads the envelope's response_text back: the
// transcript is the text, and the audio is SpeechAudio(transcript), sent in
// several deltas.
//
// Text markers script a misbehaving OpenAI. A marker anywhere in the text
// applies:
//
//   - SpeechStalls never answers the response.
//   - SpeechAnswers answers instead of reading, every time.
//   - SpeechAnswersOnce answers instead of reading on the first request for
//     a given text, reads it on the next, and so on alternately.
//   - SpeechFails sends an error event whose message carries a key-shaped
//     string, which the client must scrub.
//   - SpeechIncomplete ends the response with status "incomplete".
//   - SpeechRunsLong sends SpeechRunsLongSeconds of audio, past
//     openai.MaxSpeechSeconds.
//
// RefusedKey is refused at the handshake, as for live transcription.
const (
	SpeechStalls      = "[fake: stall]"
	SpeechAnswers     = "[fake: answer]"
	SpeechAnswersOnce = "[fake: answer once]"
	SpeechFails       = "[fake: error]"
	SpeechIncomplete  = "[fake: incomplete]"
	SpeechRunsLong    = "[fake: runs long]"
)

// SpeechAnswer is the transcript of a response that answers instead of
// reading: unrelated to any text a test reads.
const SpeechAnswer = "Sure thing. Why don't penguins use Facebook? They already have plenty of ice to break."

// SpeechRunsLongSeconds is how much audio a SpeechRunsLong response sends.
const SpeechRunsLongSeconds = openai.MaxSpeechSeconds + 1

// SpeechFailure is the error event's message; the key in it is not one.
const SpeechFailure = "The server had an error processing key sk-proj-fakeFailure123."

// SpeechBytesPerCharacter is 40 ms of 24 kHz 16-bit audio: the fake speaks
// each character of a transcript for that long.
const SpeechBytesPerCharacter = openai.SpeechSampleRate * 2 * 40 / 1000

// SpeechAudio is the PCM the fake sends for transcript:
// SpeechBytesPerCharacter bytes per character, in a fixed pattern.
func SpeechAudio(transcript string) []byte {
	return pattern(len([]rune(transcript)) * SpeechBytesPerCharacter)
}

func pattern(n int) []byte {
	pcm := make([]byte, n)
	for i := range pcm {
		pcm[i] = byte(i % 251)
	}
	return pcm
}

// SpeechAsk is one response.create the fake received.
type SpeechAsk struct {
	Voice string
	Text  string
}

type speechLog struct {
	mu   sync.Mutex
	asks []SpeechAsk
	open int
}

// speechLogs maps a key to what sessions opened with it asked for.
var speechLogs sync.Map

func logFor(key string) *speechLog {
	found, _ := speechLogs.LoadOrStore(key, &speechLog{})
	return found.(*speechLog)
}

// SpeechAsks lists the voice and text every speech session opened with key
// asked for, in order. Use a key no other test uses.
func SpeechAsks(key string) []SpeechAsk {
	log := logFor(key)
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]SpeechAsk(nil), log.asks...)
}

// OpenSpeechSessions counts speech sessions opened with key whose
// connection the fake has not yet seen close.
func OpenSpeechSessions(key string) int {
	log := logFor(key)
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.open
}

// answersOnce counts the requests per SpeechAnswersOnce text, so odd ones
// answer and even ones read.
var (
	answersOnceMu sync.Mutex
	answersOnce   = map[string]int{}
)

// IsSpeech reports whether req is the speech handshake: the Realtime
// endpoint with a model and no intent.
func IsSpeech(req *http.Request) bool {
	query := req.URL.Query()
	return req.Method == http.MethodGet && req.URL.Scheme == "https" && req.URL.Host == "api.openai.com" &&
		req.URL.Path == "/v1/realtime" && query.Has("model") && !query.Has("intent")
}

// ServeSpeech answers one speech handshake over an in-memory pipe and runs
// the session until either side closes it.
func ServeSpeech(req *http.Request) (*http.Response, error) {
	return servePipe(req, serveSpeech)
}

func serveSpeech(conn net.Conn) {
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
	if req.URL.Query().Get("model") != openai.SpeechModel {
		http.Error(writer, `{"error":{"message":"Unsupported model."}}`, http.StatusBadRequest)
		return
	}
	ws, err := websocket.Accept(writer, req, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	log := logFor(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
	log.mu.Lock()
	log.open++
	log.mu.Unlock()
	defer func() {
		log.mu.Lock()
		log.open--
		log.mu.Unlock()
	}()
	(&speechSession{session: session{ws: ws}, log: log}).run(context.Background())
}

type speechSession struct {
	session
	log   *speechLog
	voice string
}

func (s *speechSession) run(ctx context.Context) {
	for {
		_, payload, err := s.ws.Read(ctx)
		if err != nil {
			return
		}
		var event struct {
			Type     string          `json:"type"`
			Session  json.RawMessage `json:"session"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(payload, &event) != nil {
			s.fail(ctx, "invalid_json", "The event is not JSON.")
			continue
		}
		switch event.Type {
		case "session.update":
			voice, problem := checkSpeechSession(event.Session)
			if problem != "" {
				s.fail(ctx, "invalid_value", problem)
				continue
			}
			s.voice = voice
			s.send(ctx, map[string]any{"type": "session.updated", "session": event.Session})
		case "response.create":
			if s.voice == "" {
				s.fail(ctx, "invalid_value", "The session is not configured.")
				continue
			}
			text, problem := checkSpeechResponse(event.Response)
			if problem != "" {
				s.fail(ctx, "invalid_value", problem)
				continue
			}
			s.log.mu.Lock()
			s.log.asks = append(s.log.asks, SpeechAsk{Voice: s.voice, Text: text})
			s.log.mu.Unlock()
			if strings.Contains(text, SpeechStalls) {
				// Never answer; return once the client closes.
				_, _, _ = s.ws.Read(ctx)
				return
			}
			s.respond(ctx, text)
		default:
			s.fail(ctx, "invalid_event", "Unknown event type "+event.Type+".")
		}
	}
}

func (s *speechSession) respond(ctx context.Context, text string) {
	if strings.Contains(text, SpeechFails) {
		s.fail(ctx, "server_error", SpeechFailure)
		return
	}
	transcript := text
	switch {
	case strings.Contains(text, SpeechAnswers):
		transcript = SpeechAnswer
	case strings.Contains(text, SpeechAnswersOnce):
		answersOnceMu.Lock()
		answersOnce[text]++
		odd := answersOnce[text]%2 == 1
		answersOnceMu.Unlock()
		if odd {
			transcript = SpeechAnswer
		}
	}
	audio := SpeechAudio(transcript)
	if strings.Contains(text, SpeechRunsLong) {
		audio = pattern(SpeechRunsLongSeconds * openai.SpeechSampleRate * 2)
	}
	if strings.Contains(text, SpeechIncomplete) {
		audio = audio[:len(audio)/2]
	}
	// Several deltas, at most half a second each.
	chunk := min(openai.SpeechSampleRate, max(1, (len(audio)+3)/4))
	for start := 0; start < len(audio); start += chunk {
		end := min(start+chunk, len(audio))
		s.send(ctx, map[string]any{"type": "response.output_audio.delta", "delta": base64.StdEncoding.EncodeToString(audio[start:end])})
	}
	if strings.Contains(text, SpeechIncomplete) {
		s.send(ctx, map[string]any{"type": "response.done", "response": map[string]any{
			"status":         "incomplete",
			"status_details": map[string]any{"type": "incomplete", "reason": "max_output_tokens"},
		}})
		return
	}
	for _, word := range strings.SplitAfter(transcript, " ") {
		s.send(ctx, map[string]any{"type": "response.output_audio_transcript.delta", "delta": word})
	}
	s.send(ctx, map[string]any{"type": "response.output_audio_transcript.done", "transcript": transcript})
	s.send(ctx, map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}})
}

// checkSpeechSession returns the voice of a session.update shaped as
// openai.Speak sends it, or what OpenAI would refuse.
func checkSpeechSession(raw json.RawMessage) (string, string) {
	var session struct {
		Type             string   `json:"type"`
		OutputModalities []string `json:"output_modalities"`
		Instructions     string   `json:"instructions"`
		Reasoning        struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Tools      *[]json.RawMessage `json:"tools"`
		ToolChoice string             `json:"tool_choice"`
		Audio      struct {
			Input struct {
				TurnDetection json.RawMessage `json:"turn_detection"`
			} `json:"input"`
			Output struct {
				Format struct {
					Type string `json:"type"`
					Rate int    `json:"rate"`
				} `json:"format"`
				Voice string `json:"voice"`
			} `json:"output"`
		} `json:"audio"`
	}
	if json.Unmarshal(raw, &session) != nil {
		return "", "The session is not an object."
	}
	output := session.Audio.Output
	switch {
	case session.Type != "realtime":
		return "", "Only a realtime session speaks."
	case len(session.OutputModalities) != 1 || session.OutputModalities[0] != "audio":
		return "", "The output must be audio only."
	case session.Instructions != openai.SpeechInstructions:
		return "", "The instructions are not the reading instructions."
	case session.Reasoning.Effort != "minimal":
		return "", "Reasoning effort must be minimal."
	case session.Tools == nil || len(*session.Tools) != 0 || session.ToolChoice != "none":
		return "", "The session must have no tools."
	case string(session.Audio.Input.TurnDetection) != "null":
		return "", "Turn detection must be null."
	case output.Format.Type != "audio/pcm" || output.Format.Rate != openai.SpeechSampleRate:
		return "", "The audio format must be 24 kHz PCM."
	case !openai.KnownSpeechVoice(output.Voice):
		return "", "Unsupported voice."
	}
	return output.Voice, ""
}

// checkSpeechResponse returns the response_text of a response.create
// shaped as openai.Speak sends it, or what the fake refuses.
func checkSpeechResponse(raw json.RawMessage) (string, string) {
	var response struct {
		Conversation string `json:"conversation"`
		Input        []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return "", "The response is not an object."
	}
	if response.Conversation != "none" || len(response.Input) != 1 {
		return "", "The response must be out of band with one input."
	}
	message := response.Input[0]
	if message.Type != "message" || message.Role != "user" || len(message.Content) != 1 || message.Content[0].Type != "input_text" {
		return "", "The input must be one user message of text."
	}
	var envelope struct {
		RequireRepeatVerbatim bool    `json:"require_repeat_verbatim"`
		ResponseText          *string `json:"response_text"`
	}
	if json.Unmarshal([]byte(message.Content[0].Text), &envelope) != nil || !envelope.RequireRepeatVerbatim || envelope.ResponseText == nil {
		return "", "The text is not a reading envelope."
	}
	return *envelope.ResponseText, ""
}
