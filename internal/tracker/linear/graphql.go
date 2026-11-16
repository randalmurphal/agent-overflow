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

// operation is one GraphQL document.
type operation struct {
	// name is the document's operation name, which the answer's errors
	// and the tests key on.
	name string
	text string
	// mutation marks a write: it is never replayed and its answer is read
	// for its outcome. A mutation's document aliases its root field as
	// "result" and the payload's entity as "value".
	mutation bool
	// maxBody bounds the answer when the default 8 MiB is too small: a
	// page of issues carries their descriptions.
	maxBody int64
}

type graphqlRequest struct {
	Query         string `json:"query"`
	OperationName string `json:"operationName"`
	Variables     any    `json:"variables,omitempty"`
}

type envelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []wireError     `json:"errors"`
}

// payload is a mutation's result as its document aliases it.
type payload[T any] struct {
	Success bool `json:"success"`
	Value   *T   `json:"value"`
}

// call sends op with vars and decodes the answer's data into out. Its
// errors name the operation; the transport's stay reachable through
// errors.As. The Usage is the answer's, zero when there was none.
func (c *Client) call(ctx context.Context, op operation, vars, out any) (Usage, error) {
	resp, err := c.http.Do(ctx, trackerapi.Request{
		Method:       http.MethodPost,
		Path:         graphqlPath,
		Body:         graphqlRequest{Query: op.text, OperationName: op.name, Variables: vars},
		ReadOnly:     !op.mutation,
		MaxBodyBytes: op.maxBody,
	})
	if err != nil {
		return Usage{}, c.transportError(op, err)
	}
	usage := usageOf(resp.Header)
	c.recordUsage(usage)
	var env envelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return usage, fmt.Errorf("linear %s: decode the answer: %w", op.name, err)
	}
	if len(env.Errors) > 0 {
		return usage, c.answerError(op, resp.Status, env)
	}
	if len(env.Data) == 0 || bytes.Equal(env.Data, []byte("null")) {
		return usage, &Error{Op: op.name, Status: resp.Status}
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return usage, fmt.Errorf("linear %s: decode the answer's data: %w", op.name, err)
	}
	return usage, nil
}

// mutate sends a mutation and returns its payload's entity, nil when
// the payload holds none. A payload reporting success: false is a
// refused *Error.
func mutate[T any](ctx context.Context, c *Client, op operation, vars any) (*T, error) {
	var data struct {
		Result *payload[T] `json:"result"`
	}
	_, err := c.call(ctx, op, vars, &data)
	switch {
	case err != nil:
		return nil, err
	case data.Result == nil:
		return nil, &Error{Op: op.name, Status: http.StatusOK}
	case !data.Result.Success:
		return nil, &Error{Op: op.name, Status: http.StatusOK, Unsuccessful: true, Refused: true}
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
// errors becomes an *Error wrapping the transport's error, or an
// *trackerapi.AuthError when Linear refused the key under a status the
// transport did not take as a refusal.
func (c *Client) transportError(op operation, err error) error {
	statusErr, ok := errors.AsType[*trackerapi.StatusError](err)
	if !ok {
		return fmt.Errorf("linear %s: %w", op.name, err)
	}
	errs, _ := parseErrors([]byte(statusErr.Body))
	if len(errs) == 0 {
		return fmt.Errorf("linear %s: %w", op.name, err)
	}
	if auth := c.authError(statusErr.Status, errs); auth != nil {
		return fmt.Errorf("linear %s: %w", op.name, auth)
	}
	return &Error{Op: op.name, Status: statusErr.Status, Errors: errs, Refused: trackerapi.WriteNotApplied(err), Err: err}
}

// answerError types a successful HTTP answer that carries GraphQL errors.
// A mutation is refused only when it produced no payload and every error
// blames the input; otherwise its outcome is unknown.
func (c *Client) answerError(op operation, status int, env envelope) error {
	errs := make([]GraphQLError, 0, len(env.Errors))
	allUser := true
	for _, w := range env.Errors {
		e := w.typed()
		errs = append(errs, e)
		allUser = allUser && e.UserError
	}
	if auth := c.authError(status, errs); auth != nil {
		return fmt.Errorf("linear %s: %w", op.name, auth)
	}
	refused := false
	if op.mutation && allUser {
		var data struct {
			Result json.RawMessage `json:"result"`
		}
		absent := len(env.Data) == 0 || bytes.Equal(env.Data, []byte("null"))
		if !absent && json.Unmarshal(env.Data, &data) == nil {
			absent = len(data.Result) == 0 || bytes.Equal(data.Result, []byte("null"))
		}
		refused = absent
	}
	return &Error{Op: op.name, Status: status, Errors: errs, Refused: refused}
}

// authError is the refusal of the key that an answer's errors name, or
// nil.
func (c *Client) authError(status int, errs []GraphQLError) *trackerapi.AuthError {
	for _, e := range errs {
		if e.Code == CodeAuthentication {
			return &trackerapi.AuthError{Tracker: "linear", Method: http.MethodPost, URL: c.endpoint, Status: status, Message: joinErrors(errs)}
		}
	}
	return nil
}
