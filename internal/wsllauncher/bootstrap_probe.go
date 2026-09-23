package wsllauncher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"agent-overflow/internal/startupprogress"
)

// These classes describe evidence from the bootstrap probe, not a guessed
// network cause.
var (
	ErrBackendUnreachable = errors.New("no HTTP response from the WSL backend over Windows localhost")
	ErrInvalidBootstrap   = errors.New("unexpected backend startup response")
	ErrBackendNotReady    = errors.New("backend did not finish starting")
)

// BootstrapHTTPError is a bootstrap answer other than 200 or 503: the
// backend is reachable and refused or failed.
type BootstrapHTTPError struct {
	StatusCode int
	URL        string
}

func (e BootstrapHTTPError) Error() string {
	return fmt.Sprintf("GET %s: status %d", e.URL, e.StatusCode)
}

// BackendStalledError reports a backend that sent startup progress and then
// stopped advancing it for Quiet. It matches ErrBackendNotReady.
type BackendStalledError struct {
	// Progress is the last report the backend sent.
	Progress startupprogress.Progress
	Quiet    time.Duration
	// Last is the final attempt's failure: a 503 or a transport error.
	Last error
}

func (e *BackendStalledError) Error() string {
	return fmt.Sprintf("%v: no progress for %s in phase %s (%s): %v", ErrBackendNotReady, e.Quiet, e.Progress.Phase, e.Progress.Status(), e.Last)
}

func (e *BackendStalledError) Unwrap() error { return ErrBackendNotReady }

// bootstrapProbeAttemptTimeout caps one HTTP attempt. A refused connection
// answers in milliseconds; a timeout means the request reached the kernel
// and the server did not answer, which is rare and recoverable.
const bootstrapProbeAttemptTimeout = 1 * time.Second

// bootstrapProbeDeadline is the longest the probe waits without evidence of
// progress: no HTTP response, a bare 503, or a starting report whose
// updatedAt stopped advancing. WSL2 NAT mode installs the Windows-side
// forward rule for a fresh listener after it binds, so the first attempts
// are often refused while the backend is healthy.
const bootstrapProbeDeadline = 30 * time.Second

// bootstrapProbePollInterval caps the gap between attempts. The gap starts
// at bootstrapProbeInitialPollInterval and doubles, because the backend
// publishes its port before it is ready and a failed attempt is cheap.
const (
	bootstrapProbePollInterval        = 250 * time.Millisecond
	bootstrapProbeInitialPollInterval = 25 * time.Millisecond
)

// bootstrapBodyLimit bounds a bootstrap answer while leaving room for the
// full manifest.
const bootstrapBodyLimit = 64 << 10

// ProbeConfig tunes ProbeBootstrap. Zero durations take the production
// defaults.
type ProbeConfig struct {
	AttemptTimeout time.Duration
	// Deadline is the longest the probe waits without progress.
	Deadline time.Duration
	// PollInterval caps the retry gap; InitialPollInterval is the first
	// gap, doubling after every failed attempt up to the cap.
	PollInterval        time.Duration
	InitialPollInterval time.Duration
	// OnProgress receives every starting report in arrival order.
	OnProgress func(startupprogress.Progress)

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func (cfg ProbeConfig) withDefaults() ProbeConfig {
	if cfg.AttemptTimeout <= 0 {
		cfg.AttemptTimeout = bootstrapProbeAttemptTimeout
	}
	if cfg.Deadline <= 0 {
		cfg.Deadline = bootstrapProbeDeadline
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = bootstrapProbePollInterval
	}
	if cfg.InitialPollInterval <= 0 {
		cfg.InitialPollInterval = bootstrapProbeInitialPollInterval
	}
	cfg.InitialPollInterval = min(cfg.InitialPollInterval, cfg.PollInterval)
	if cfg.OnProgress == nil {
		cfg.OnProgress = func(startupprogress.Progress) {}
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.sleep == nil {
		cfg.sleep = sleepContext
	}
	return cfg
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ProbeBootstrap polls the backend's authenticated /bootstrap.json over
// Windows localhost until it answers the manifest this launch expects.
//
// A 503 is retried. A starting report counts as progress whenever its
// updatedAt changes, so a long boot that keeps reporting never fails; the
// probe gives up after Deadline without progress. The failure is
// ErrBackendUnreachable when no HTTP response ever arrived, a
// BackendStalledError after a starting report, and ErrBackendNotReady for
// a backend that only answered the bare 503. Any other status is terminal
// as a BootstrapHTTPError; an unexpected manifest is ErrInvalidBootstrap.
func ProbeBootstrap(ctx context.Context, port int, token string, cfg ProbeConfig) error {
	cfg = cfg.withDefaults()
	// 127.0.0.1, not "localhost": Windows resolves "localhost" to ::1 as
	// well, and WSL2's localhostForwarding only proxies IPv4.
	target := fmt.Sprintf("http://127.0.0.1:%d/bootstrap.json", port)
	log.Printf("probe: GET %s (token=%d bytes)", target, len(token))

	client := &http.Client{Timeout: cfg.AttemptTimeout}
	lastAdvance := cfg.now()
	var (
		lastErr         error
		attempt         int
		sawHTTPResponse bool
		reported        *startupprogress.Progress
	)
	wait := cfg.InitialPollInterval
	for {
		attempt++
		resp, err := GetWithToken(ctx, client, target, token)
		switch {
		case err != nil:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("probe GET %s: %w", target, ctxErr)
			}
			lastErr = err
		default:
			sawHTTPResponse = true
			body, _ := io.ReadAll(io.LimitReader(resp.Body, bootstrapBodyLimit))
			_ = resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				if err := validateBootstrapResponse(body, port); err != nil {
					log.Printf("probe: invalid bootstrap response: %v", err)
					return fmt.Errorf("%w: %w", ErrInvalidBootstrap, err)
				}
				if attempt > 1 {
					log.Printf("probe: ok after %d attempts", attempt)
				}
				return nil
			case http.StatusServiceUnavailable:
				lastErr = fmt.Errorf("GET %s: status %d", target, resp.StatusCode)
				if p, ok := startupprogress.Parse(resp.StatusCode, body); ok {
					if reported == nil || p.UpdatedAt != reported.UpdatedAt {
						lastAdvance = cfg.now()
					}
					if reported == nil || p.Phase != reported.Phase || p.Step != reported.Step {
						log.Printf("probe: backend starting: phase=%s step=%d/%d updating_to=%q", p.Phase, p.Step, p.Steps, p.UpdatingTo)
					}
					reported = &p
					cfg.OnProgress(p)
				}
			default:
				// Reachable but refused. The status and the first bytes of
				// the body go to the log only.
				log.Printf("probe: status=%d host-resp=%q", resp.StatusCode, string(body[:min(len(body), 256)]))
				return BootstrapHTTPError{StatusCode: resp.StatusCode, URL: target}
			}
		}
		if quiet := cfg.now().Sub(lastAdvance); quiet >= cfg.Deadline {
			switch {
			case !sawHTTPResponse:
				return fmt.Errorf("GET %s: %w after %d attempts: %w", target, ErrBackendUnreachable, attempt, lastErr)
			case reported != nil:
				return &BackendStalledError{Progress: *reported, Quiet: quiet, Last: lastErr}
			default:
				return fmt.Errorf("%w: GET %s timed out after %d attempts: %w", ErrBackendNotReady, target, attempt, lastErr)
			}
		}
		if err := cfg.sleep(ctx, wait); err != nil {
			return fmt.Errorf("probe GET %s: %w", target, err)
		}
		wait = min(wait*2, cfg.PollInterval)
	}
}

// GetWithToken issues one launcher request. The session token rides an
// Authorization header: the query slot on this route belongs to a
// browser's one-time page ticket, and a header keeps the credential out of
// logged URLs.
func GetWithToken(ctx context.Context, client *http.Client, target, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return client.Do(req)
}

// validateBootstrapResponse checks that the manifest belongs to the backend
// this launch booted. The wsUrl's host and port prove the responder is our
// backend on our port rather than another server on the forwarded hop.
func validateBootstrapResponse(body []byte, port int) error {
	var bootstrap struct {
		WSURL string `json:"wsUrl"`
	}
	if err := json.Unmarshal(body, &bootstrap); err != nil {
		return fmt.Errorf("decode bootstrap response: %w", err)
	}
	parsed, err := url.Parse(bootstrap.WSURL)
	if err != nil {
		return fmt.Errorf("parse bootstrap wsUrl: %w", err)
	}
	if parsed.Scheme != "ws" || parsed.Path != "/ws" {
		return fmt.Errorf("bootstrap wsUrl has unexpected shape: %q", bootstrap.WSURL)
	}
	host, portString, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return fmt.Errorf("split bootstrap wsUrl host: %w", err)
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("bootstrap wsUrl host = %q, want 127.0.0.1", host)
	}
	if portString != fmt.Sprintf("%d", port) {
		return fmt.Errorf("bootstrap wsUrl port = %q, want %d", portString, port)
	}
	return nil
}
