// Package openai calls OpenAI's API with the user's own API key
// (internal/openaikey).
//
// Every keyed request goes to api.openai.com and nowhere else: the host is a
// constant here, not a setting, so no configuration can point the key at
// another server. The key travels only in the Authorization header. What
// OpenAI answers is never passed on verbatim where it could carry the key:
// a refused key gets a fixed message, and any other message is scrubbed of
// anything shaped like a key before it reaches an error.
package openai

import (
	"errors"
	"net/http"
	"regexp"

	"agent-overflow/internal/secret"
)

// apiHost is the only host a keyed request reaches.
const apiHost = "api.openai.com"

// ErrKeyRefused means OpenAI refused the API key.
var ErrKeyRefused = errors.New("OpenAI refused the API key; check the key in its file")

// ErrRateLimited means OpenAI is limiting the key's requests, or its
// account is out of credit.
var ErrRateLimited = errors.New("OpenAI is limiting this API key's requests, or its account is out of credit")

// keyShaped matches OpenAI key prefixes (sk-, sk-proj-, sess-) and what
// follows them, including the masked form OpenAI echoes in a refusal.
var keyShaped = regexp.MustCompile(`\b(?:sk|sess)-[A-Za-z0-9_*\-]+`)

// scrub removes anything shaped like a key from text OpenAI sent.
func scrub(text string) string {
	return keyShaped.ReplaceAllString(text, "[key]")
}

// authorization is the header carrying the key. net/http holds header
// values as strings, so this copy lives until the request is collected;
// the caller still zeroes its secret.Bytes.
func authorization(key *secret.Bytes) http.Header {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+string(key.Reveal()))
	return header
}
