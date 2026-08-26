package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Client is the mock-side half of the control wire. All methods are
// best-effort against a live backend; the mock must keep functioning
// (fallback scenario, no live driving) when the backend is gone —
// a dead control plane must never hang a provider process the app is
// still reading from.
type Client struct {
	base   string
	token  string
	mockID string
	http   *http.Client
}

// FromEnv builds a client from the env vars the harness backend set at
// boot. ok is false when they're absent — the mock is running outside
// a harness (a bare Go test, a curious human) and should use its
// fallback scenario source.
func FromEnv() (c *Client, ok bool) {
	addr := os.Getenv(EnvAddr)
	token := os.Getenv(EnvToken)
	if addr == "" || token == "" {
		return nil, false
	}
	return &Client{
		base:  "http://" + addr,
		token: token,
		// Poll requests hold up to longPollWindow server-side; give the
		// client margin above that so healthy long-polls never error.
		http: &http.Client{Timeout: longPollWindow + 10*time.Second},
	}, true
}

// Register announces the mock and receives its scenario assignment.
// Must be called before Poll/Report.
func (c *Client) Register(reg Registration) (RegisterResponse, error) {
	body, err := json.Marshal(reg)
	if err != nil {
		return RegisterResponse{}, fmt.Errorf("control: marshal registration: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/register", bytes.NewReader(body))
	if err != nil {
		return RegisterResponse{}, err
	}
	var resp RegisterResponse
	if err := c.do(req, &resp); err != nil {
		return RegisterResponse{}, fmt.Errorf("control: register: %w", err)
	}
	c.mockID = resp.MockID
	return resp, nil
}

// Poll long-polls for commands until ctx is cancelled, invoking handle
// for each command in order on the polling goroutine. Transient errors
// back off and retry; the loop exits with ctx, or when the backend
// stops recognising this mock.
//
// The second exit is the important one. A harness reset drops every
// registration, and a mock that outlives it gets 404 "unknown mock" on
// every poll forever after — which used to be treated as transient, so
// the process spun at 1Hz logging a failure per second for the rest of
// its life. There is no recovery: registration happens once, at boot.
// The mock keeps running its scenario standalone, exactly as it does
// when the control vars were never set.
func (c *Client) Poll(ctx context.Context, handle func(Command)) {
	for {
		if ctx.Err() != nil {
			return
		}
		cmds, err := c.pollOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrUnknownMock) {
				log.Printf("mockprovider: control channel dropped this mock (%s); continuing standalone", c.mockID)
				return
			}
			log.Printf("mockprovider: control poll: %v", err)
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		for _, cmd := range cmds {
			handle(cmd)
		}
	}
}

func (c *Client) pollOnce(ctx context.Context) ([]Command, error) {
	u := c.base + "/commands?mock=" + url.QueryEscape(c.mockID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var cmds []Command
	if err := c.do(req, &cmds); err != nil {
		return nil, err
	}
	return cmds, nil
}

// Report sends a progress event. Failures are logged and swallowed —
// progress reporting must never stall scenario execution.
func (c *Client) Report(rep Report) {
	body, err := json.Marshal(rep)
	if err != nil {
		log.Printf("mockprovider: marshal report: %v", err)
		return
	}
	u := c.base + "/report?mock=" + url.QueryEscape(c.mockID)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		log.Printf("mockprovider: build report request: %v", err)
		return
	}
	if err := c.do(req, nil); err != nil {
		log.Printf("mockprovider: report: %v", err)
	}
}

func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.token)
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		trimmed := bytes.TrimSpace(msg)
		// The server answers a request naming a mock it has no
		// registration for with this exact body. A bare 404 is NOT enough
		// to conclude it: the auth wrapper also answers 404 (the
		// don't-fingerprint convention) and a bad token is worth retrying
		// no more or less than any other transport failure — but it is not
		// this condition, and conflating them would make a token typo look
		// like a reset.
		if resp.StatusCode == http.StatusNotFound && bytes.Equal(trimmed, []byte(unknownMockBody)) {
			return fmt.Errorf("%w: %s %s", ErrUnknownMock, req.Method, req.URL.Path)
		}
		return fmt.Errorf("%s %s: %s (%s)", req.Method, req.URL.Path, resp.Status, trimmed)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
