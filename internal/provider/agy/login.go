package agy

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
)

const (
	antigravityCodeRedirectURI = "https://antigravity.google/oauth-callback"
	maxAuthorizationURLLen     = 4096
	loginURLReadTimeout        = 30 * time.Second
)

var (
	// ErrInvalidLoginCode reports a pasted value that is neither a valid OAuth
	// authorization code nor a matching Antigravity callback URL.
	ErrInvalidLoginCode = errors.New("enter the sign-in code or callback URL shown on the Antigravity page")
	validLoginCodeRE    = regexp.MustCompile(`^[\w./~-]+$`)
)

// LoginConfig configures one `agy acp --login` sign-in process targeting an
// isolated temporary directory.
type LoginConfig struct {
	Binary  string
	WorkDir string
	// Home is the isolated home the sign-in writes its credential into.
	Home   Home
	Env    provider.EnvInput
	Reaper procutil.Reaper
}

// LoginSession drives `agy acp --login` over stdio: captures the printed Google
// OAuth URL, accepts the pasted authorization code or callback URL on stdin,
// and waits for `agy` to persist `antigravity-oauth-token` and exit 0.
type LoginSession struct {
	proc       *provider.Process
	authURL    string
	authState  string
	redirect   string
	waitDone   chan error
	outputTail *lockedOutputTail
	closeOnce  sync.Once
}

// StartLogin spawns `agy acp --login` against an isolated Gemini home and waits
// until the process prints a validated Google OAuth authorization URL.
func StartLogin(ctx context.Context, cfg LoginConfig) (*LoginSession, error) {
	binary := ResolveBinary(cfg.Binary)
	if strings.TrimSpace(binary) == "" {
		return nil, errors.New("agy login: binary path is empty")
	}
	if !cfg.Home.Isolated() {
		return nil, errors.New("agy login: an isolated home is required")
	}
	workDir := cfg.WorkDir
	if strings.TrimSpace(workDir) == "" {
		workDir = "."
	}

	// An isolated home is on file storage, so the login lands in its
	// `antigravity-oauth-token` instead of the user's global OS keyring slot.
	launch, err := cfg.Home.Launch(cfg.Env)
	if err != nil {
		return nil, fmt.Errorf("agy login: %w", err)
	}

	proc, err := provider.Spawn(ctx, provider.Watched(cfg.Reaper), provider.SpawnConfig{
		Binary:         binary,
		Args:           launch.ACPArgs("--login"),
		Dir:            workDir,
		Env:            launch.Env,
		Provider:       string(provider.Agy),
		ThreadID:       "agy-login",
		GracefulCancel: true,
	})
	if err != nil {
		return nil, fmt.Errorf("agy login: start %s: %w", binary, err)
	}

	urlCh := make(chan parsedAuthURL, 1)
	tail := &lockedOutputTail{}
	var urlOnce sync.Once
	go func() {
		for {
			lineBytes, readErr := proc.ReadLine()
			if len(lineBytes) > 0 {
				line := string(lineBytes)
				tail.append(line)
				if parsed, ok := extractAuthorizationURLFromLine(line); ok {
					urlOnce.Do(func() {
						urlCh <- parsed
					})
				}
			}
			if readErr != nil {
				// Also check stderr tail in case agy printed the URL to stderr.
				for _, stderrLine := range strings.Split(proc.StderrTail(), "\n") {
					if parsed, ok := extractAuthorizationURLFromLine(stderrLine); ok {
						urlOnce.Do(func() {
							urlCh <- parsed
						})
					}
				}
				return
			}
		}
	}()

	waitDone := make(chan error, 1)
	go func() {
		<-proc.Done()
		waitDone <- proc.Err()
	}()

	urlCtx, cancel := context.WithTimeout(ctx, loginURLReadTimeout)
	defer cancel()

	select {
	case parsed := <-urlCh:
		return &LoginSession{
			proc:       proc,
			authURL:    parsed.rawURL,
			authState:  parsed.state,
			redirect:   parsed.redirectURI,
			waitDone:   waitDone,
			outputTail: tail,
		}, nil
	case err := <-waitDone:
		_ = proc.Close()
		detail := strings.TrimSpace(firstNonEmpty(tail.summary(), proc.StderrTail()))
		if detail != "" {
			return nil, fmt.Errorf("agy login exited before providing a sign-in link: %s", detail)
		}
		if err != nil {
			return nil, fmt.Errorf("agy login exited before providing a sign-in link: %w", err)
		}
		return nil, errors.New("agy login exited before providing a sign-in link")
	case <-urlCtx.Done():
		_ = proc.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("agy login timed out waiting for the Google sign-in link")
	}
}

// AuthorizeURL returns the validated Google OAuth URL printed by `agy acp --login`.
func (s *LoginSession) AuthorizeURL() string {
	return s.authURL
}

// ValidatePaste checks whether pasted is either an authorization code or a
// callback URL matching this sign-in session's state and redirect_uri.
func (s *LoginSession) ValidatePaste(pasted string) (string, error) {
	return validateLoginPaste(pasted, s.redirect, s.authState)
}

// SubmitCode sends the validated code or callback URL to `agy acp --login` on
// stdin and waits for the process to finish writing the OAuth token.
func (s *LoginSession) SubmitCode(ctx context.Context, pasted string) error {
	value, err := s.ValidatePaste(pasted)
	if err != nil {
		return err
	}
	if err := s.proc.WriteLine([]byte(value)); err != nil {
		return fmt.Errorf("agy login: send sign-in code: %w", err)
	}
	select {
	case err := <-s.waitDone:
		if err != nil {
			detail := strings.TrimSpace(firstNonEmpty(s.outputTail.summary(), s.proc.StderrTail()))
			if detail != "" {
				return fmt.Errorf("agy login failed: %s", detail)
			}
			return fmt.Errorf("agy login failed: %w", err)
		}
		return nil
	case <-ctx.Done():
		_ = s.Close()
		return ctx.Err()
	}
}

// Close terminates the login subprocess if still running.
func (s *LoginSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.proc != nil {
			err = s.proc.Close()
		}
	})
	return err
}

type parsedAuthURL struct {
	rawURL      string
	redirectURI string
	state       string
}

func extractAuthorizationURLFromLine(line string) (parsedAuthURL, bool) {
	for _, field := range strings.Fields(line) {
		candidate := strings.Trim(field, "\"'`<>()[]")
		if !strings.HasPrefix(candidate, "https://accounts.google.com/") {
			continue
		}
		if parsed, ok := parseGoogleAuthorizationURL(candidate); ok {
			return parsed, true
		}
	}
	return parsedAuthURL{}, false
}

func parseGoogleAuthorizationURL(raw string) (parsedAuthURL, bool) {
	if len(raw) == 0 || len(raw) > maxAuthorizationURLLen || strings.ContainsAny(raw, " \t\r\n") {
		return parsedAuthURL{}, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return parsedAuthURL{}, false
	}
	if u.Scheme != "https" || u.Host != "accounts.google.com" || u.User != nil || u.Fragment != "" {
		return parsedAuthURL{}, false
	}
	if u.Path != "/o/oauth2/auth" && u.Path != "/o/oauth2/v2/auth" {
		return parsedAuthURL{}, false
	}
	q := u.Query()
	if len(q["response_type"]) != 1 || q.Get("response_type") != "code" {
		return parsedAuthURL{}, false
	}
	if len(q["state"]) != 1 {
		return parsedAuthURL{}, false
	}
	state := q.Get("state")
	if state == "" || len(state) > 512 {
		return parsedAuthURL{}, false
	}
	if len(q["redirect_uri"]) != 1 {
		return parsedAuthURL{}, false
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI != antigravityCodeRedirectURI {
		return parsedAuthURL{}, false
	}
	return parsedAuthURL{
		rawURL:      raw,
		redirectURI: redirectURI,
		state:       state,
	}, true
}

func validateLoginPaste(pasted, expectedRedirect, expectedState string) (string, error) {
	value := strings.TrimSpace(pasted)
	if value == "" {
		return "", ErrInvalidLoginCode
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", ErrInvalidLoginCode
	}
	if len(value) <= 2048 && validLoginCodeRE.MatchString(value) {
		return value, nil
	}
	if len(value) > maxAuthorizationURLLen || strings.ContainsAny(value, " \t") {
		return "", ErrInvalidLoginCode
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil {
		return "", ErrInvalidLoginCode
	}
	base := u.Scheme + "://" + u.Host + u.Path
	if expectedRedirect != "" && base != expectedRedirect {
		return "", ErrInvalidLoginCode
	}
	q := u.Query()
	if len(q["code"]) != 1 || strings.TrimSpace(q.Get("code")) == "" {
		return "", ErrInvalidLoginCode
	}
	if expectedState != "" && (len(q["state"]) != 1 || q.Get("state") != expectedState) {
		return "", ErrInvalidLoginCode
	}
	return value, nil
}

type lockedOutputTail struct {
	mu    sync.Mutex
	lines []string
}

func (l *lockedOutputTail) append(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) >= 8 {
		l.lines = l.lines[1:]
	}
	l.lines = append(l.lines, line)
}

func (l *lockedOutputTail) summary() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, " | ")
}
