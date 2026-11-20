package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// graphqlPath is the GraphQL endpoint under the base URL.
const graphqlPath = "graphql"

type graphqlRequest struct {
	Query         string `json:"query"`
	OperationName string `json:"operationName"`
	Variables     any    `json:"variables,omitempty"`
}

// payload is a mutation's result as its document aliases it: its
// success, of whatever JSON type Linear sent, and the entity aliased
// "value".
type payload[T any] struct {
	Success any `json:"success"`
	Value   *T  `json:"value"`
}

// mutationData is a mutation answer's data: the root field aliased
// "result".
type mutationData[T any] struct {
	Result *payload[T] `json:"result"`
}

func (d *mutationData[T]) state() payloadState {
	if d.Result == nil {
		return payloadNone
	}
	return successState(d.Result.Success)
}

// payloadStater is a mutation answer's data, which says the write's
// outcome. Every mutation's out implements it.
type payloadStater interface{ state() payloadState }

// reply is what a call reports beside its data.
type reply struct {
	Usage  Usage
	Status int
	// Served is Linear's clock when it answered, from the answer's Date
	// header; zero when the answer carried none that parses.
	Served time.Time
}

// answer is the answer an error is built from. scrub removes the
// request's credential from any text taken from it.
type answer struct {
	op     operation
	status int
	scrub  func(string) string
}

// call sends op with vars and decodes the answer's data into out in one
// pass over the body. A mutation's out is a payloadStater. Its errors
// name the operation; the transport's stay reachable through errors.As.
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
	r := reply{Usage: usageOf(resp.Header), Status: resp.Status, Served: servedAt(resp.Header)}
	a := answer{op: op, status: resp.Status, scrub: resp.Scrub}
	present, wire, err := decodeAnswer(resp.Body, out)
	if err != nil {
		return r, a.decodeError("decode the answer", err)
	}
	outcome := payloadNone
	if op.mutation && present {
		outcome = out.(payloadStater).state()
	}
	if len(wire) > 0 || (op.mutation && outcome != payloadSucceeded) {
		return r, c.failure(a, wire, outcome, nil)
	}
	if !present {
		return r, &Error{Op: op.name, Status: resp.Status}
	}
	return r, nil
}

// servedAt is the time an answer's Date header names, zero when it has
// none that parses.
func servedAt(h http.Header) time.Time {
	t, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return time.Time{}
	}
	return t
}

// decodeOptions read an answer as encoding/json does where Linear's
// answers could differ: text that is not valid UTF-8 becomes U+FFFD and
// a repeated member keeps its last value.
var decodeOptions = jsonv2.JoinOptions(jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))

// decodeAnswer reads a GraphQL answer in one pass: its errors, and its
// data straight into out, so the data is never held as raw bytes.
// present reports data that is neither absent nor null; out is
// untouched when it is not.
func decodeAnswer(body []byte, out any) (present bool, wire []wireError, err error) {
	dec := jsontext.NewDecoder(bytes.NewBuffer(body), decodeOptions)
	if tok, err := dec.ReadToken(); err != nil {
		return false, nil, err
	} else if tok.Kind() != '{' {
		return false, nil, errors.New("the answer is not a JSON object")
	}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return false, nil, err
		}
		switch name.String() {
		case "data":
			if dec.PeekKind() == 'n' {
				_, err = dec.ReadToken()
				present = false
				break
			}
			present = true
			err = jsonv2.UnmarshalDecode(dec, out, decodeOptions)
		case "errors":
			err = jsonv2.UnmarshalDecode(dec, &wire, decodeOptions)
		default:
			err = dec.SkipValue()
		}
		if err != nil {
			return false, nil, err
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return false, nil, err
	}
	return present, wire, nil
}

// payloadState is what a mutation answer's data says about the write.
type payloadState int

const (
	// payloadNone is no result: data or the result absent or null.
	payloadNone payloadState = iota
	// payloadUnsaid is a result without a boolean success: missing,
	// null or another type.
	payloadUnsaid
	payloadFailed    // success: false
	payloadSucceeded // success: true
)

// successState reads a payload's success as decoded into an any.
func successState(v any) payloadState {
	switch v {
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
	var data mutationData[T]
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
	var data mutationData[json.RawMessage]
	present, wire, derr := decodeAnswer([]byte(statusErr.Body), &data)
	if derr != nil || len(wire) == 0 {
		return fmt.Errorf("linear %s: %w", op.name, err)
	}
	outcome := payloadNone
	if op.mutation && present {
		outcome = data.state()
	}
	return c.failure(answer{op: op, status: statusErr.Status, scrub: statusErr.Scrub}, wire, outcome, err)
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
