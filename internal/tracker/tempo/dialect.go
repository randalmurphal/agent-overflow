package tempo

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// dialect is Tempo's error and limit conventions.
//
// Errors: a 400 or 404 carries {"errors": ...} as JSON. The API overview
// shows errors as one object, {"errors": {"message": "..."}}; the
// ErrorBean schema has a list, {"errors": [{"message": "..."}]}. Both are
// read. Tempo names no field, so Problem.Fields stays empty. A 401 and a
// 403 are text/html and carry no message.
//
// Limits: Tempo signals a limit only with a 429, which the transport
// handles; the answer documents no Retry-After, and the transport's
// backoff of at least one second matches Tempo's advice to wait a second.
// A base URL for the wrong region answers with an authorization error,
// which is a 401 like any refused token, so no answer is marked as a
// refused login beyond what its status says.
type dialect struct{}

var _ trackerapi.Dialect = dialect{}

func (dialect) RejectedLogin(http.Header) bool { return false }

func (dialect) RateLimited(int, http.Header, []byte) (time.Time, bool) {
	return time.Time{}, false
}

func (dialect) Problem(_ int, body []byte) trackerapi.Problem {
	var envelope struct {
		Errors json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Errors) == 0 {
		return trackerapi.Problem{}
	}
	type message struct {
		Message string `json:"message"`
	}
	var messages []string
	var list []message
	var one message
	switch {
	case json.Unmarshal(envelope.Errors, &list) == nil:
		for _, m := range list {
			if text := strings.TrimSpace(m.Message); text != "" {
				messages = append(messages, text)
			}
		}
	case json.Unmarshal(envelope.Errors, &one) == nil:
		if text := strings.TrimSpace(one.Message); text != "" {
			messages = append(messages, text)
		}
	}
	return trackerapi.Problem{Message: strings.Join(messages, "; ")}
}
