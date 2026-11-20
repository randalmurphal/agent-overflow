package jira

import (
	"encoding/json"

	"agent-overflow/internal/tracker/trackerapi"
)

// AnswerError is a 2xx answer this package could not read: a body that
// is not JSON, or JSON of another shape than the call's, or for a write a
// status or body its spec does not document. Applied is set only on a
// write's answer that is a documented success naming what was written
// (see write): the write happened, and the result carries what decoded.
// Any other AnswerError of a write leaves its outcome unknown. Message
// holds no text of the answer that could carry the credential.
type AnswerError struct {
	Message string
	Applied bool
}

func (e *AnswerError) Error() string { return "jira: " + e.Message }

// decodeAnswer decodes resp's body into out. The transport hands over
// the body already scrubbed of the credential the request carried, in
// every string and object key (trackerapi.Response.Body), so no text a
// call returns, puts in an error or lists as a problem or warning can
// repeat it. A body that is not one JSON value of out's shape is an
// *AnswerError.
func decodeAnswer(resp *trackerapi.Response, out any) error {
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return &AnswerError{Message: "the answer did not decode: " + resp.Scrub(err.Error())}
	}
	return nil
}
