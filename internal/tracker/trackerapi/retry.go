package trackerapi

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// settle decides an answer: the call is done (with a nil error for a
// success) or goes again.
func (c *Client) settle(ctx context.Context, k *call, state *retries, a *answer) (bool, error) {
	switch {
	case a.refused:
		return c.afterRefusal(ctx, k, state, a)
	case a.limited:
		return c.afterLimit(ctx, k, state, a)
	case a.status == http.StatusServiceUnavailable && k.read:
		failure := c.statusError(k, a)
		if state.count >= maxRetries {
			return false, failure
		}
		wait, ok := retryAfter(a.header, c.now())
		if !ok {
			wait = backoff(state.count)
		}
		return c.retryWait(ctx, k, state, min(wait, maxRetryWait), failure)
	case a.status >= 400:
		return false, c.statusError(k, a)
	}
	return false, nil
}

func (c *Client) statusError(k *call, a *answer) *StatusError {
	problem := c.dialect.Problem(a.status, a.body)
	body := a.body
	if len(body) > maxErrorBody {
		body = body[:maxErrorBody]
	}
	return &StatusError{
		Tracker: c.name, Method: k.method, URL: redact(a.url), Status: a.status,
		Message: problem.Message, Fields: problem.Fields, Body: string(body),
	}
}

// afterRefusal reads the credential again after the first refusal and
// sends the call once more when that replaced the credential the refusal
// answered. A refusal marked on an answer that is not a 4xx may have run
// a write, so a write goes again only after a 4xx.
func (c *Client) afterRefusal(ctx context.Context, k *call, state *retries, a *answer) (bool, error) {
	refusal := &AuthError{
		Tracker: c.name, Method: k.method, URL: redact(a.url), Status: a.status,
		Message: c.dialect.Problem(a.status, a.body).Message,
	}
	if state.reread || (!k.read && !clientError(a.status)) {
		return false, refusal
	}
	state.reread = true
	if err := c.loadCredential(ctx, true, a.sentGen); err != nil {
		if ctx.Err() != nil {
			err = c.ended(ctx, c.where(k.method, k.target))
		}
		refusal.Err = err
		return false, refusal
	}
	c.credMu.RLock()
	unchanged := c.cred == a.sentTo
	c.credMu.RUnlock()
	if unchanged {
		return false, refusal
	}
	return true, nil
}

// afterLimit closes the gate and fails the call, unless it is interactive,
// may be sent again, has retries left and the gate opens soon enough: the
// next attempt then waits for the gate in admit. A write is sent again
// only after a 429, which the tracker answers before it applies anything.
func (c *Client) afterLimit(ctx context.Context, k *call, state *retries, a *answer) (bool, error) {
	now := c.now()
	until := a.until
	if until.IsZero() {
		until = now.Add(backoff(state.count))
	}
	until = c.gate.close(until)
	limited := &RateLimitedError{Tracker: c.name, Until: until, Status: a.status}
	wait := until.Sub(now)
	resend := k.read || a.status == http.StatusTooManyRequests
	if !resend || !IsInteractive(ctx) || state.count >= maxRetries || wait > interactiveGateWait || pastDeadline(ctx, wait) {
		return false, limited
	}
	state.count++
	return true, nil
}

// afterFailure retries a read after a transient failure to send or to
// read the answer, unless Stream already wrote to its destination.
func (c *Client) afterFailure(ctx context.Context, k *call, state *retries, err error) (bool, error) {
	if _, transient := errors.AsType[*TransientError](err); !transient || !k.read || k.committed || ctx.Err() != nil || state.count >= maxRetries {
		return false, err
	}
	return c.retryWait(ctx, k, state, backoff(state.count), err)
}

// retryWait waits before the next attempt, or fails with failure when
// the wait would outlast the call.
func (c *Client) retryWait(ctx context.Context, k *call, state *retries, wait time.Duration, failure error) (bool, error) {
	if pastDeadline(ctx, wait) {
		return false, failure
	}
	if err := c.sleep(ctx, wait); err != nil {
		return false, c.ended(ctx, c.where(k.method, k.target))
	}
	state.count++
	return true, nil
}
