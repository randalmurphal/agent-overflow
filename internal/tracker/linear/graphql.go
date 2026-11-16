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

// payload is a mutation's result as its document aliases it. Success is
// a pointer so that a missing or null success is told from false.
type payload[T any] struct {
	Success *bool `json:"success"`
	Value   *T    `json:"value"`
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
	if len(env.Errors) > 0 {
		return r, c.failure(a, env.Errors, env.Data, nil)
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

// mutate sends a mutation and returns its payload's entity, nil when
// the payload holds none. Only a present success: false is a refusal;
// a payload that is missing or does not say it succeeded leaves the
// outcome unknown.
func mutate[T any](ctx context.Context, c *Client, op operation, vars any) (*T, error) {
	var data struct {
		Result *payload[T] `json:"result"`
	}
	r, err := c.call(ctx, op, vars, &data)
	switch {
	case err != nil:
		return nil, err
	case data.Result == nil:
		return nil, &Error{Op: op.name, Status: r.Status}
	case data.Result.Success == nil:
		return nil, fmt.Errorf("linear %s: the payload does not say whether the mutation succeeded", op.name)
	case !*data.Result.Success:
		return nil, &Error{Op: op.name, Status: r.Status, Unsuccessful: true, Refused: true}
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
// wrong. It is a *trackerapi.NotSentError, so WriteNotApplied holds.
func invalid(call, format string, args ...any) error {
	return &trackerapi.NotSentError{Err: fmt.Errorf("linear "+call+": "+format, args...)}
}

// transportError types a failed call. An error answer carrying GraphQL
// errors becomes an *Error wrapping the transport's error. Its body is
// the transport's, already scrubbed.
func (c *Client) transportError(op operation, err error) error {
	statusErr, ok := errors.AsType[*trackerapi.StatusError](err)
	if !ok {
		return fmt.Errorf("linear %s: %w", op.name, err)
	}
	wire, _ := parseErrors([]byte(statusErr.Body))
	if len(wire) == 0 {
		return fmt.Errorf("linear %s: %w", op.name, err)
	}
	return c.failure(answer{op: op, status: statusErr.Status, scrub: unscrubbed}, wire, nil, err)
}

// failure builds the error of an answer that carries GraphQL errors;
// every error built from answer text passes through it or decodeError.
// The key refused under a status the transport did not take as a
// refusal is an *trackerapi.AuthError. A write is refused when the
// transport's error proves it (an HTTP 4xx) or when it produced no
// payload and every error blames the input; otherwise its outcome is
// unknown.
func (c *Client) failure(a answer, wire []wireError, data json.RawMessage, transportErr error) error {
	errs := typedErrors(wire, a.scrub)
	for _, e := range errs {
		if e.Code == CodeAuthentication {
			auth := &trackerapi.AuthError{Tracker: "linear", Method: http.MethodPost, URL: c.endpoint, Status: a.status, Message: joinErrors(errs)}
			return fmt.Errorf("linear %s: %w", a.op.name, auth)
		}
	}
	e := &Error{Op: a.op.name, Status: a.status, Errors: errs, Err: transportErr}
	allUser := true
	for _, g := range errs {
		allUser = allUser && g.UserError
		e.notFound = e.notFound || g.notFound(a.op.root)
	}
	switch {
	case transportErr != nil:
		e.Refused = trackerapi.WriteNotApplied(transportErr)
	case a.op.mutation && allUser:
		e.Refused = payloadAbsent(data)
	}
	return e
}

// payloadAbsent reports a mutation answer whose data holds no result.
func payloadAbsent(data json.RawMessage) bool {
	if absent(data) {
		return true
	}
	var d struct {
		Result json.RawMessage `json:"result"`
	}
	return json.Unmarshal(data, &d) == nil && absent(d.Result)
}

// decodeError is an answer that did not decode. The decoder's message
// can quote the answer, so it is scrubbed and carried as text.
func (a answer) decodeError(what string, err error) error {
	return fmt.Errorf("linear %s: %s: %s", a.op.name, what, a.scrub(err.Error()))
}
