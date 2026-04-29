// Package wsllauncher detects WSL distributions and spawns the
// agent-overflow Linux backend inside a chosen distro. The Windows-only
// pieces (Job Object lifetime control) live in launcher_windows.go;
// distro parsing and the public interface are cross-platform so unit
// tests run on macOS via captured fixture bytes.
//
// WSL2 forwards 127.0.0.1:<port> bound inside the distro to the Windows
// host's localhost. The Windows-side WebView2 connects to
// http://localhost:<port> and the loopback forwarder reaches the WSL
// process. localhostForwarding=true must be set in /etc/wsl.conf or
// %USERPROFILE%/.wslconfig; this is the WSL2 default but a user can
// disable it. We document the requirement and surface a clear error if
// the launcher's WS connection back to the WSL backend fails.
package wsllauncher

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf16"
)

// listDistrosTimeout caps how long ListDistros waits for wsl.exe to
// emit its inventory. The picker UI presents an error if WSL is
// installed but the call hangs (a known failure mode when the WSL VM
// is in a broken state).
const listDistrosTimeout = 5 * time.Second

// runListDistrosCmd executes a pre-configured `wsl.exe -l -v` cmd
// and returns the parsed distro list. Both the Windows-host
// ListDistros (HideWindow SysProcAttr) and the WSL-side ListDistros
// (no SysProcAttr — it's a Linux child of a Linux backend) build
// their own *exec.Cmd to keep platform-specific options visible at
// the call site, then hand the cmd off here for the run + parse.
//
// Returns ([], nil) when wsl.exe reports the localized "no installed
// distributions" message in stderr; the picker UI uses the empty
// slice as the cue to show install instructions.
func runListDistrosCmd(cmd *exec.Cmd) ([]Distro, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			// wsl.exe writes its diagnostic messages in UTF-16 LE on
			// most modern Windows builds. Decode best-effort so we
			// can differentiate "no distros installed" from a
			// structural failure (vmcompute down, kernel update
			// needed, etc).
			text, _ := decodeUTF16LE(stderr.Bytes())
			if text == "" {
				text = stderr.String()
			}
			if isNoDistrosMessage(text) {
				return nil, nil
			}
			return nil, fmt.Errorf("wsllauncher: wsl.exe -l -v failed: %w (stderr: %s)", err, strings.TrimSpace(text))
		}
		return nil, fmt.Errorf("wsllauncher: run wsl.exe -l -v: %w", err)
	}
	return parseDistroList(out)
}

// isNoDistrosMessage returns true when wsl.exe's stderr indicates the
// "WSL is installed but no distros" path. The English string is the
// most common; we match a few localised forms loosely so French /
// German / Japanese hosts don't fall into the unrecognised-failure
// branch.
//
// Matched substrings:
//   - "no installed distributions" (en-US)
//   - "There are no" + "distributions" (loose for localized "There are no <X> distributions...")
//   - "WSL_E_DEFAULT_DISTRO_NOT_FOUND" (raw error code wsl.exe sometimes emits)
func isNoDistrosMessage(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	if lower == "" {
		return false
	}
	switch {
	case strings.Contains(lower, "no installed distributions"):
		return true
	case strings.Contains(lower, "wsl_e_default_distro_not_found"):
		return true
	case strings.Contains(lower, "there are no") && strings.Contains(lower, "distribut"):
		return true
	}
	return false
}

// Distro describes one WSL distribution as reported by wsl.exe -l -v.
//
// The wsl.exe output is fixed-width, with a leading "* " marker on the
// default distro and a single character of indent for non-defaults.
// Version is parsed numerically (1 for WSL1, 2 for WSL2). State is the
// raw column ("Running", "Stopped", "Installing", etc) — we don't
// normalise it because WSL has added new states over time and a literal
// pass-through is robust to that.
//
// JSON tags: lowercased to match the rest of the wire-bound types in
// this codebase (see settings.NetworkSettings, store.Item, etc). The
// App method ListWSLDistros returns []Distro, which the Wails binding
// generator surfaces to TypeScript using these tag names.
type Distro struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
	Version int    `json:"version"`
	State   string `json:"state"`
}

// ErrMalformedUTF16 is returned by parseDistroList when the input is
// non-empty but cannot be decoded as UTF-16 LE. wsl.exe's stdout is
// UTF-16 LE with a BOM; anything else is a sign the caller is parsing
// the wrong stream (e.g. captured stderr, or an unrelated binary).
var ErrMalformedUTF16 = errors.New("wsllauncher: malformed UTF-16 LE output")

// parseDistroList decodes wsl.exe -l -v stdout (UTF-16 LE with BOM)
// into a slice of Distro records. Returns an empty slice + nil error
// when the input is empty (WSL not installed, or the wsl.exe call
// returned no rows).
//
// We deliberately keep the parser tolerant: the column widths shift
// across Windows versions, so we treat "two-or-more spaces" as the
// column separator instead of fixed offsets. This matches the parser
// strategy that CodexMonitor and similar Tauri apps use in practice.
func parseDistroList(raw []byte) ([]Distro, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	text, err := decodeUTF16LE(raw)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return nil, nil
	}

	out := make([]Distro, 0, len(lines))
	headerSeen := false
	for _, line := range lines {
		// wsl.exe pads every column with spaces and emits CR before LF.
		// strings.TrimRight strips CR; the leading "* " or "  " marker
		// stays so the default-flag check below can find it.
		line = strings.TrimRight(line, "\r ")
		if line == "" {
			continue
		}

		// The header row starts with whitespace then "NAME". Skip it
		// once and only once — a non-English Windows install localises
		// the header (e.g. "NOM" on French Windows), so we fall back
		// to "first row whose VERSION column doesn't parse as int".
		trimmed := strings.TrimSpace(line)
		isDefault := false
		if strings.HasPrefix(line, "*") {
			isDefault = true
			trimmed = strings.TrimSpace(strings.TrimPrefix(line, "*"))
		}

		fields := splitColumns(trimmed)
		if len(fields) < 3 {
			// Unrecognised row; skip rather than error so a malformed
			// trailing line doesn't kill the list.
			continue
		}
		// Treat anything where the third column isn't a number as a
		// header row. This covers both English ("VERSION") and any
		// localised label.
		ver, ok := parseInt(fields[2])
		if !ok {
			if !headerSeen {
				headerSeen = true
				continue
			}
			continue
		}

		out = append(out, Distro{
			Name:    fields[0],
			Default: isDefault,
			Version: ver,
			State:   fields[1],
		})
	}
	return out, nil
}

// decodeUTF16LE decodes a UTF-16 LE byte slice with an optional BOM
// into a Go string. Returns ErrMalformedUTF16 if the byte length is
// odd (UTF-16 code units are always 2 bytes) or if the BOM is the
// big-endian variant — wsl.exe always writes LE, so seeing BE means
// the caller is reading the wrong stream.
func decodeUTF16LE(raw []byte) (string, error) {
	// Strip BOM if present. WSL's actual output starts with FF FE.
	switch {
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		raw = raw[2:]
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		// Big-endian BOM — not what wsl.exe emits.
		return "", fmt.Errorf("%w: big-endian BOM (expected little-endian)", ErrMalformedUTF16)
	}

	if len(raw)%2 != 0 {
		return "", fmt.Errorf("%w: odd byte length %d", ErrMalformedUTF16, len(raw))
	}

	codeUnits := make([]uint16, len(raw)/2)
	if err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, &codeUnits); err != nil {
		return "", fmt.Errorf("%w: %v", ErrMalformedUTF16, err)
	}
	return string(utf16.Decode(codeUnits)), nil
}

// splitColumns splits a row on runs of two-or-more spaces, returning
// the trimmed column values. This is more robust than strings.Fields
// (which would split "Linux 22.04" into two columns) and more robust
// than fixed offsets (which break across Windows versions).
func splitColumns(line string) []string {
	out := make([]string, 0, 3)
	inField := false
	start := 0
	spaceRun := 0
	for i, r := range line {
		if r == ' ' {
			if inField {
				spaceRun++
				if spaceRun >= 2 {
					out = append(out, strings.TrimSpace(line[start:i-1]))
					inField = false
				}
			}
			continue
		}
		if !inField {
			start = i
			inField = true
		}
		spaceRun = 0
	}
	if inField {
		out = append(out, strings.TrimSpace(line[start:]))
	}
	return out
}

// parseInt is a tiny stdlib-free integer parser for the version
// column. Returns (0, false) for non-numeric input.
func parseInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}
