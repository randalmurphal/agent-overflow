package trackertools

import (
	"encoding/base64"
	"encoding/json"
)

// A cursor is the opaque text an answer gives the model to continue it
// (ticket_find's next, ticket_read's more_comments): base64url of a small
// JSON document. It names connections and issues by ID, which no answer
// shows otherwise. A cursor the model altered decodes to a refusal, or to a
// read the tracker refuses; it reaches nothing a key could not.

// maxCursorBytes bounds a cursor accepted back: every cursor these tools
// write is a connection ID, an issue ID, a tracker page token and a few
// numbers.
const maxCursorBytes = 2048

func encodeCursor(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// Every cursor is a struct of strings and integers.
		panic("trackertools: encode a cursor: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor reads text, which field of the call carried, into v.
func decodeCursor(field, text string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if len(text) > maxCursorBytes || err != nil || json.Unmarshal(raw, v) != nil {
		return invalidf("%s is not one these tools gave; leave it out to start again.", field)
	}
	return nil
}
