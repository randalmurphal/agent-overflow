// Package harnessclient is the Go client for a running agent test
// harness (or soak) instance: how to find one, how to start one, and how
// to speak the transport wire to it.
//
// It is the twin of e2e/src/harness.ts, kept importable so Go tests and
// cmd/ao-harness share one implementation of the bootstrap contract and
// the frame handling. It links no App code and no transport server code:
// everything here is what a foreign process can observe — a JSON line on
// stdout, a 0600 file in the data dir, and one WebSocket.
//
// The frame shapes below mirror internal/transport/frame.go. They are
// restated rather than imported so a CLI does not link the server; a
// drift guard in the tests decodes this package's frames through the
// transport structs, so the two cannot disagree silently.
package harnessclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

// BootstrapPrefix marks the harness bootstrap line on the backend's
// stdout. Same spelling as main's harnessStdoutPrefix; a launcher
// scanning stdout matches on it verbatim.
const BootstrapPrefix = "__AO_HARNESS__:"

// Bootstrap is everything needed to attach to an instance. It is the
// payload of the stdout line AND the whole content of
// <dataDir>/harness-instance.json, which additionally carries the
// identity block (empty on a line parsed off stdout, since the id is
// assigned after the line is written).
type Bootstrap struct {
	URL          string `json:"url"`
	Port         int    `json:"port"`
	Token        string `json:"token"`
	DataRoot     string `json:"dataRoot"`
	DataDir      string `json:"dataDir"`
	HomeDir      string `json:"homeDir,omitempty"`
	MockProvider string `json:"mockProvider"`
	PID          int    `json:"pid"`
	Version      string `json:"version"`
	PageMarker   string `json:"pageMarker,omitempty"`
	// StartupError is set when App.Start failed. The transport still
	// serves so logs are readable, but the instance is not usable.
	StartupError string `json:"startupError,omitempty"`

	// Identity ties the payload to a registry row. Embedded from the
	// package that writes it, so a field added there reaches this reader
	// without a second declaration.
	instanceinfo.Identity
}

// ValidateFor checks a bootstrap payload against the data root the caller
// selected. A payload from another root must never be attached just because
// its port happens to answer.
func (b Bootstrap) ValidateFor(dataRoot, dataDir string) error {
	if err := b.Identity.Validate(dataRoot, dataDir); err != nil {
		return err
	}
	if b.DataRoot != "" {
		want, err := instanceinfo.CanonicalPath(dataRoot)
		if err != nil {
			return err
		}
		got, err := instanceinfo.CanonicalPath(b.DataRoot)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("harnessclient: bootstrap data root %q does not match selected root %q", b.DataRoot, dataRoot)
		}
	}
	if b.DataDir != "" && dataDir != "" {
		want, err := instanceinfo.CanonicalPath(dataDir)
		if err != nil {
			return err
		}
		got, err := instanceinfo.CanonicalPath(b.DataDir)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("harnessclient: bootstrap data dir %q does not match selected dir %q", b.DataDir, dataDir)
		}
	}
	return nil
}

// WSURL is the authenticated WebSocket endpoint for this instance. The
// token rides the query because a WebSocket client cannot set request
// headers in every environment this URL is used from (the browser and
// Node clients in e2e most of all); the transport validates it through
// the same function that validates a page cookie.
func (b Bootstrap) WSURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d/ws?token=%s", b.Port, url.QueryEscape(b.Token))
}

// pageURLPath is the transport route that answers a page URL carrying a
// fresh one-time ticket. Restated rather than imported, like the frame
// shapes above: this package must stay linkable without the server. A
// drift guard in the tests compares it to transport.PageURLPath.
const pageURLPath = "/pageurl"

// pageURLTimeout bounds one page-URL request. The instance is on
// loopback and answers the route from memory, so this is a stall budget
// for a CLI command, not a network one.
const pageURLTimeout = 5 * time.Second

// maxPageURLBytes bounds the response body: one URL and a newline. 8 KiB
// is far past any real page URL and caps a misbehaving responder.
const maxPageURLBytes = 8192

// PageURL asks a live instance for a page URL carrying a freshly minted
// one-time ticket, which is what a browser exchanges for its session
// cookie on first contact.
//
// The URL recorded in the instance file was minted at boot and is spent
// by the first page that loads it, so any command that hands a human or
// a browser a URL to open asks for a new one rather than reusing that
// one. The session token goes in a header: the query slot on the
// transport's routes belongs to the page ticket, and a header keeps the
// credential out of process listings and logs.
func (b Bootstrap) PageURL(ctx context.Context) (string, error) {
	if b.Port == 0 || b.Token == "" {
		return "", errors.New("harnessclient: bootstrap carries no port/token")
	}
	ctx, cancel := context.WithTimeout(ctx, pageURLTimeout)
	defer cancel()
	endpoint := fmt.Sprintf("http://127.0.0.1:%d%s", b.Port, pageURLPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", endpoint, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageURLBytes))
	if err != nil {
		return "", err
	}
	pageURL := strings.TrimSpace(string(body))
	if pageURL == "" {
		return "", errors.New("harnessclient: page url response was empty")
	}
	return pageURL, nil
}

// InstanceFilePath names the data-dir file that carries a live
// instance's bootstrap payload.
func InstanceFilePath(dataDir string) string {
	return filepath.Join(dataDir, instanceinfo.InstanceFileName)
}

// ReadInstanceFile attaches to an already-running instance by reading
// the file its boot published. This is the path that does not require
// having spawned the backend: the token lives in a 0600 file inside the
// data root, so anyone who can open the data root can attach.
func ReadInstanceFile(dataDir string) (Bootstrap, error) {
	path := InstanceFilePath(dataDir)
	if info, err := os.Lstat(dataDir); err != nil {
		return Bootstrap{}, fmt.Errorf("inspect data dir %s: %w", dataDir, err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Bootstrap{}, fmt.Errorf("refusing non-directory or symlinked data dir %s", dataDir)
	}
	if info, err := os.Lstat(path); err != nil {
		return Bootstrap{}, fmt.Errorf("inspect %s: %w", path, err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Bootstrap{}, fmt.Errorf("refusing non-regular or symlinked instance file %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("read %s: %w", path, err)
	}
	var bs Bootstrap
	if err := json.Unmarshal(data, &bs); err != nil {
		return Bootstrap{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if bs.Port == 0 || bs.Token == "" {
		return Bootstrap{}, fmt.Errorf("%s names no port or token; the instance is not attachable", path)
	}
	if bs.IdentityVersion != 0 {
		if err := bs.ValidateFor(bs.DataRoot, bs.DataDir); err != nil {
			return Bootstrap{}, fmt.Errorf("validate %s: %w", path, err)
		}
	}
	return bs, nil
}

// ParseBootstrapLine extracts the payload from one line of backend
// stdout. ok is false for any line that is not a bootstrap line, so a
// caller can feed it every line it reads; a line that carries the prefix
// but will not parse is an error rather than a miss, because that is a
// broken contract and not noise.
func ParseBootstrapLine(line string) (bs Bootstrap, ok bool, err error) {
	at := strings.Index(line, BootstrapPrefix)
	if at < 0 {
		return Bootstrap{}, false, nil
	}
	payload := strings.TrimSpace(line[at+len(BootstrapPrefix):])
	if err := json.Unmarshal([]byte(payload), &bs); err != nil {
		return Bootstrap{}, false, fmt.Errorf("unparseable harness bootstrap line %q: %w", line, err)
	}
	return bs, true, nil
}

// scanBootstrap reads lines until one carries the bootstrap payload.
// Returns io.EOF when the stream ended without one — the caller knows
// far more than this function does about why (the process exited, the
// deadline passed) and owns the message.
func scanBootstrap(r io.Reader, onLine func(string)) (Bootstrap, error) {
	scanner := bufio.NewScanner(r)
	// The payload is a single JSON object with a handful of paths in it;
	// 1 MiB is orders of magnitude of headroom and bounds a stdout that
	// turns out to be something else entirely.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if onLine != nil {
			onLine(line)
		}
		bs, ok, err := ParseBootstrapLine(line)
		if err != nil {
			return Bootstrap{}, err
		}
		if ok {
			return bs, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return Bootstrap{}, err
	}
	return Bootstrap{}, io.EOF
}
