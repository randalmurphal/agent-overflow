package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
)

// ForgeCall is one gh or glab invocation, forwarded by the fake forge CLI
// (cmd/ao-mockforge) that an isolated boot runs in place of the real
// ones. The fake is a pipe: the harness answers the call from its forge
// fixture (internal/harness/forgefake) and the fake prints the answer.
type ForgeCall struct {
	// CLI is the forge CLI the call was made to: "gh" or "glab".
	CLI string `json:"cli"`
	// Args is the argument vector after the program name, verbatim.
	Args []string `json:"args"`
	// Cwd is the working directory the app ran the CLI in. Empty when the
	// fake could not read it.
	Cwd string `json:"cwd"`
	// Stdin is everything the app wrote to the CLI's standard input.
	Stdin []byte `json:"stdin,omitempty"`
	PID   int    `json:"pid"`
}

// ForgeResult is what the fake writes back: stdout bytes (possibly
// binary), stderr text and the exit status.
type ForgeResult struct {
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exitCode"`
}

// maxForgeCallBytes bounds a /forge request body. The app writes at most
// a review payload on stdin; anything larger is a broken caller.
const maxForgeCallBytes = 16 << 20

// noForgeBody answers /forge on a server built without a Forge handler.
const noForgeBody = "this harness has no fake forge"

// Forge forwards one forge CLI invocation and returns the harness's
// answer. Unlike Report this is not best-effort: the caller is standing
// in for a CLI the app is waiting on, and an unreachable harness must
// read as that CLI failing.
func (c *Client) Forge(call ForgeCall) (ForgeResult, error) {
	body, err := json.Marshal(call)
	if err != nil {
		return ForgeResult{}, fmt.Errorf("control: marshal forge call: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/forge", bytes.NewReader(body))
	if err != nil {
		return ForgeResult{}, err
	}
	var result ForgeResult
	if err := c.do(req, &result); err != nil {
		return ForgeResult{}, fmt.Errorf("control: forge: %w", err)
	}
	return result, nil
}

func (s *Server) handleForge(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Forge == nil {
		http.Error(w, noForgeBody, http.StatusServiceUnavailable)
		return
	}
	var call ForgeCall
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxForgeCallBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&call); err != nil {
		http.Error(w, "bad forge call: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "bad forge call: trailing data", http.StatusBadRequest)
		return
	}
	result := s.cfg.Forge(call)
	if err := writeJSON(w, result); err != nil {
		log.Printf("harness control: %s (pid %d) never received its forge answer: %v", call.CLI, call.PID, err)
	}
}
