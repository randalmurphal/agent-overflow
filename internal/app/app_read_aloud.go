package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"agent-overflow/internal/openai"
	"agent-overflow/internal/openaikey"
	"agent-overflow/internal/readaloud"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/transport"
)

// Read aloud in OpenAI voices: with Settings > Voice's switch on and a key
// in its file, a client reading a reply aloud sends each sentence here
// (MintReadAloudTicket) and fetches its audio from the URL it gets back.
// The fetch is what calls OpenAI with the key (internal/openai) and answers
// a WAV file (transport's speech route). Nothing is stored, and the key is
// read only for each connection's handshake.

// readAloudTimeout bounds one fetch's synthesis, retry included.
const readAloudTimeout = 45 * time.Second

// ReadAloudVoice is one OpenAI voice a reply can be read in.
type ReadAloudVoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ReadAloudSourceStatus reports whether this computer reads replies aloud
// in OpenAI voices.
type ReadAloudSourceStatus struct {
	// Available is true when the Settings > Voice switch is on here and the
	// key file holds a usable key.
	Available bool `json:"available"`
	// Voices lists the voices, always, so a settings page can show them
	// before the switch is on.
	Voices []ReadAloudVoice `json:"voices"`
}

// readAloudAPIOn is the Settings > Voice switch. Off by default: it sends
// each sentence to OpenAI with this computer's key.
func (a *App) readAloudAPIOn() bool {
	return a.settings != nil && a.settings.BackendScreen().Get().ReadAloudAPIEnabled
}

// ReadAloudSource reports whether replies can be read aloud in OpenAI
// voices on this computer, and in which voices.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) ReadAloudSource() (ReadAloudSourceStatus, error) {
	voices := openai.SpeechVoices()
	status := ReadAloudSourceStatus{Voices: make([]ReadAloudVoice, 0, len(voices))}
	for _, voice := range voices {
		status.Voices = append(status.Voices, ReadAloudVoice{ID: voice.ID, Name: voice.Name})
	}
	if !a.readAloudAPIOn() {
		return status, nil
	}
	file, err := a.openAIKeyFile()
	if err != nil {
		return ReadAloudSourceStatus{}, err
	}
	status.Available = file.State == openaikey.StatePresent
	return status, nil
}

// MintReadAloudTicket admits one sentence to be spoken in voice and returns
// the relative URL the audio is fetched from once.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) MintReadAloudTicket(ctx context.Context, text, voice string) (string, error) {
	if !a.readAloudAPIOn() {
		return "", readaloud.ErrOff
	}
	server := a.transportServer.Load()
	if server == nil {
		return "", errors.New("read aloud: transport is not serving")
	}
	sentences := a.readAloudSentences()
	id, err := sentences.Admit(text, voice)
	if err != nil {
		return "", err
	}
	url, err := server.MintSpeechTicket(id)
	if err != nil {
		if _, takeErr := sentences.Take(id); takeErr != nil {
			return "", errors.Join(err, takeErr)
		}
		return "", err
	}
	return url, nil
}

func (a *App) readAloudSentences() *readaloud.Sentences {
	a.readAloudOnce.Do(func() { a.readAloud = readaloud.New(transport.SpeechTicketTTL) })
	return a.readAloud
}

// ReadAloudTransfer adapts admitted sentences onto the transport's speech
// route. A package function, not a method, so it never becomes an RPC.
func ReadAloudTransfer(a *App) transport.SpeechTransfer { return readAloudTransfer{app: a} }

type readAloudTransfer struct{ app *App }

func (t readAloudTransfer) SpeakSentence(ctx context.Context, speechID string) ([]byte, error) {
	a := t.app
	request, err := a.readAloudSentences().Take(speechID)
	if err != nil {
		return nil, &transport.SpeechError{Status: http.StatusNotFound, Err: err}
	}
	if !a.readAloudAPIOn() {
		return nil, &transport.SpeechError{Status: http.StatusConflict, Err: readaloud.ErrOff}
	}
	speaking, cancel := context.WithTimeout(ctx, readAloudTimeout)
	defer cancel()
	speech, err := a.readAloudSentences().Synthesize(speaking, request, a.speakWithOpenAI)
	if err != nil {
		return nil, readAloudFailure(ctx, err)
	}
	wav, err := readaloud.WAV(speech.PCM, openai.SpeechSampleRate)
	if err != nil {
		return nil, &transport.SpeechError{Status: http.StatusBadGateway, Err: err}
	}
	return wav, nil
}

func (a *App) speakWithOpenAI(ctx context.Context, request openai.SpeechRequest) (openai.Speech, error) {
	return speakOnce(ctx, a.openAIHTTPClient(), a.readOpenAIKey, request)
}

// speakOnce reads one request aloud on a connection of its own. The key is
// read for the handshake and zeroed once that is done, before OpenAI
// speaks.
func speakOnce(ctx context.Context, client *http.Client, readKey func() (*secret.Bytes, error), request openai.SpeechRequest) (openai.Speech, error) {
	key, err := readKey()
	if err != nil {
		return openai.Speech{}, &transport.SpeechError{Status: http.StatusConflict, Err: err}
	}
	session, err := openai.DialSpeech(ctx, client, key, request.Voice)
	key.Zero()
	if err != nil {
		return openai.Speech{}, err
	}
	defer session.Close()
	return session.Speak(ctx, request.Text)
}

// readAloudFailure gives a failed synthesis the status the route answers.
// request is the fetch's own context, to tell its end from the time limit.
func readAloudFailure(request context.Context, err error) error {
	var classified *transport.SpeechError
	switch {
	case request.Err() != nil, errors.As(err, &classified):
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return &transport.SpeechError{Status: http.StatusGatewayTimeout,
			Err: fmt.Errorf("OpenAI took longer than %d seconds to read the sentence aloud", int(readAloudTimeout.Seconds()))}
	case errors.Is(err, readaloud.ErrBusy), errors.Is(err, openai.ErrRateLimited):
		return &transport.SpeechError{Status: http.StatusTooManyRequests, Err: err}
	}
	return &transport.SpeechError{Status: http.StatusBadGateway, Err: err}
}
