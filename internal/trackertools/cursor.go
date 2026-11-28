package trackertools

import (
	"encoding/base64"
	"encoding/json"
)

// A cursor is the opaque text an answer gives the model to continue it
// (ticket_find's next, a value of ticket_read's more): base64url of a small
// JSON document. It names connections and issues by ID, which no answer
// shows otherwise. A cursor the model altered decodes to a refusal, or to a
// read the tracker refuses; it reaches nothing a key could not.

// maxCursorBytes bounds a cursor accepted back. Every cursor these tools
// write is a connection ID, an issue ID, a tracker page token and a few
// numbers; the largest page token is a Linear listing's, which names up
// to 100 issue IDs of a tie (linear.Position), under 6 KB as a cursor.
const maxCursorBytes = 8192

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
	refused := invalidf("%s is not one these tools gave; leave it out to start again.", field)
	if len(text) > maxCursorBytes {
		return refused
	}
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || json.Unmarshal(raw, v) != nil {
		return refused
	}
	return nil
}
