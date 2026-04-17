package replay

import (
	"encoding/json"
	"time"
)

// Record is a single line written to a thread's replay log.
//
// The shape is deliberately minimal: a timestamp (unix milliseconds), the
// thread id, a short channel name describing what kind of event this is,
// and an opaque Data field carrying whatever the caller wants to record.
// Data is stored as a json.RawMessage so we can echo provider payloads
// without double-encoding.
type Record struct {
	Timestamp int64           `json:"ts"`
	ThreadID  string          `json:"threadId"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// NewRecord constructs a Record. If ts is zero, it is replaced with the
// current time in unix milliseconds. Data is marshalled if not already a
// json.RawMessage; an error is returned for un-marshallable inputs so the
// caller doesn't silently drop events.
func NewRecord(ts time.Time, threadID, kind string, data any) (Record, error) {
	if ts.IsZero() {
		ts = time.Now()
	}
	rec := Record{
		Timestamp: ts.UnixMilli(),
		ThreadID:  threadID,
		Kind:      kind,
	}
	if data == nil {
		return rec, nil
	}
	if raw, ok := data.(json.RawMessage); ok {
		rec.Data = raw
		return rec, nil
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return Record{}, err
	}
	rec.Data = encoded
	return rec, nil
}
