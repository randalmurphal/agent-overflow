package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"agent-overflow/internal/tracker/trackerapi"
)

// graphqlPath is the GraphQL endpoint under the base URL.
const graphqlPath = "graphql"

type graphqlRequest struct {
	Query         string `json:"query"`
	OperationName string `json:"operationName"`
	Variables     any    `json:"variables,omitempty"`
}

type envelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []wireError     `json:"errors"`
}

// payload is a mutation's result as its document aliases it; its
// success is read by readPayload.
type payload[T any] struct {
	Value *T `json:"value"`
}

// reply is what a call reports beside its data.
type reply struct {
	Usage  Usage
	Status int
}

// answer is the answer an error is built from. scrub removes the
// request's credential from any text taken from it.
type answer struct {
	op     operation
	status int
	scrub  func(string) string
}

// call sends op with vars and decodes the answer's data into out. Its
// errors name the operation; the transport's stay reachable through
// errors.As.
func (c *Client) call(ctx context.Context, op operation, vars, out any) (reply, error) {
	resp, err := c.http.Do(ctx, trackerapi.Request{
		Method:       http.MethodPost,
		Path:         graphqlPath,
		Body:         graphqlRequest{Query: op.text, OperationName: op.name, Variables: vars},
		ReadOnly:     !op.mutation,
		MaxBodyBytes: op.maxBody,
	})
	if err != nil {
		return reply{}, c.transportError(op, err)
	}
	// The client's Usage was recorded by observeAnswer; this answer's
	// own is read from its headers so a page keeps it under concurrency.
	r := reply{Usage: usageOf(resp.Header), Status: resp.Status}
	a := answer{op: op, status: resp.Status, scrub: resp.Scrub}
	var env envelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return r, a.decodeError("decode the answer", err)
	}
	outcome := payloadNone
	if op.mutation {
		outcome = readPayload(env.Data)
	}
	if len(env.Errors) > 0 || (op.mutation && outcome != payloadSucceeded) {
		return r, c.failure(a, env.Errors, outcome, nil)
	}
	if absent(env.Data) {
		return r, &Error{Op: op.name, Status: resp.Status}
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return r, a.decodeError("decode the answer's data", err)
	}
	return r, nil
}

func absent(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

// payloadState is what a mutation answer's data says about the write.
type payloadState int

const (
	// payloadNone is no result: data or the result absent or null.
	payloadNone payloadState = iota
	// payloadUnsaid is a result without a boolean success: missing,
	// null or malformed.
	payloadUnsaid
	payloadFailed    // success: false
	payloadSucceeded // success: true
)

// readPayload reads the success of a mutation answer's result.
func readPayload(data json.RawMessage) payloadState {
	if absent(data) {
		return payloadNone
	}
	var d struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(data, &d) != nil {
		return payloadUnsaid
	}
	if absent(d.Result) {
		return payloadNone
	}
	var p struct {
		Success any `json:"success"`
	}
	if json.Unmarshal(d.Result, &p) != nil {
		return payloadUnsaid
	}
	switch p.Success {
	case true:
		return payloadSucceeded
	case false:
		return payloadFailed
	}
	return payloadUnsaid
}

// mutate sends a mutation and returns its payload's entity, nil when
// the payload holds none. call returns no error only for a payload
// that says success: true; failure classifies every other answer.
func mutate[T any](ctx context.Context, c *Client, op operation, vars any) (*T, error) {
	var data struct {
		Result *payload[T] `json:"result"`
	}
	if _, err := c.call(ctx, op, vars, &data); err != nil {
		return nil, err
	}
	return data.Result.Value, nil
}

// mutateStored is mutate for a payload that must carry the stored
// entity; a success without it leaves the outcome unknown.
func mutateStored[W, T any](ctx context.Context, c *Client, op operation, vars any, typed func(W) T) (*T, error) {
	w, err := mutate[W](ctx, c, op, vars)
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, fmt.Errorf("linear %s: the payload reported success without the stored entity", op.name)
	}
	v := typed(*w)
	return &v, nil
}

// invalid is a call refused before sending, for an input it can tell is
// wrong (ErrNoRoom included). It is a *trackerapi.NotSentError, so
// WriteNotApplied holds, and matches trackerapi.ErrInvalidInput, which
// tells it from a call the transport could not send.
func invalid(call, format string, args ...any) error {
	return trackerapi.InvalidInput(fmt.Errorf("linear "+call+": "+format, args...))
}

// transportError types a failed call. An error answer carrying GraphQL
// errors becomes an *Error wrapping the transport's error. Its body was
// scrubbed as text, which decoding can undo (a JSON escape inside the
// key), so what is decoded from it is scrubbed again by failure, with
// the error's own Scrub.
func (c *Client) transportError(op operation, err error) error {
	statusErr, ok := errors.AsType[*trackerapi.StatusError](err)
	if !ok {
		return fmt.Errorf("linear %s: %w", op.name, err)
	}
	var env envelope
	if json.Unmarshal([]byte(statusErr.Body), &env) != nil || len(env.Errors) == 0 {
		return fmt.Errorf("linear %s: %w", op.name, err)
	}
	outcome := payloadNone
	if op.mutation {
		outcome = readPayload(env.Data)
	}
	return c.failure(answer{op: op, status: statusErr.Status, scrub: statusErr.Scrub}, env.Errors, outcome, err)
}

// failure builds the error of a GraphQL answer that failed: one with
// errors, or a mutation's without success: true. It is the one place
// decoded answer text becomes error text, through a.scrub, whatever the
// status; decodeError is the other path, for an answer that did not
// decode. The key refused under a status the transport did not take as
// a refusal is an *trackerapi.AuthError; with success: false it is the
// Err of a refused *Error.
//
// A write is refused when its payload says success: false, with or
// without errors; when the transport's error proves it (an HTTP 4xx);
// or, on an answer the transport passed, when there is no payload and
// every error blames the input. Every other outcome is unknown.
func (c *Client) failure(a answer, wire []wireError, outcome payloadState, transportErr error) error {
	errs := typedErrors(wire, a.scrub)
	unsuccessful := a.op.mutation && outcome == payloadFailed
	cause := transportErr
	for _, g := range errs {
		if g.Code != CodeAuthentication {
			continue
		}
		auth := &trackerapi.AuthError{Tracker: "linear", Method: http.MethodPost, URL: c.endpoint, Status: a.status, Message: joinErrors(errs)}
		if !unsuccessful {
			return fmt.Errorf("linear %s: %w", a.op.name, auth)
		}
		// success: false still proves the refusal; the AuthError stays
		// reachable through errors.As.
		cause = auth
		break
	}
	e := &Error{Op: a.op.name, Status: a.status, Errors: errs, Err: cause}
	allUser := len(errs) > 0
	for _, g := range errs {
		allUser = allUser && g.UserError
		e.notFound = e.notFound || g.notFound(a.op.root)
	}
	switch {
	case unsuccessful:
		e.Unsuccessful, e.Refused = true, true
	case transportErr != nil:
		e.Refused = trackerapi.WriteNotApplied(transportErr)
	case a.op.mutation && outcome == payloadNone:
		e.Refused = allUser
	case a.op.mutation && outcome == payloadUnsaid && len(errs) == 0:
		e.Err = errors.New("the payload does not say whether the mutation succeeded")
	}
	return e
}

// decodeError is an answer that did not decode. The decoder's message
// can quote the answer, so it is scrubbed and carried as text.
func (a answer) decodeError(what string, err error) error {
	return fmt.Errorf("linear %s: %s: %s", a.op.name, what, a.scrub(err.Error()))
}
