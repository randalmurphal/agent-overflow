// Package instanceinfo is the discovery layer for running harness and soak
// instances. It owns the instance identity model, registry rows, and liveness
// records used by the harness CLI.
package instanceinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Mode string

const (
	ModeHarness Mode = "harness"
	ModeSoak    Mode = "soak"
	ModePerf    Mode = "perf"
)

const InstanceFileName = "harness-instance.json"
const IdentityVersion = 1
const idHexLen = 8

type Identity struct {
	IdentityVersion          int    `json:"identityVersion,omitempty"`
	ID                       string `json:"id"`
	Mode                     Mode   `json:"mode"`
	Window                   bool   `json:"window"`
	BootNonce                string `json:"bootNonce,omitempty"`
	ProcessStartTime         string `json:"processStartTime,omitempty"`
	ExecutablePath           string `json:"executablePath,omitempty"`
	PIDNamespace             string `json:"pidNamespace,omitempty"`
	Worktree                 string `json:"worktree"`
	StartedAt                string `json:"startedAt"`
	LauncherPid              int    `json:"launcherPid,omitempty"`
	LauncherProcessStartTime string `json:"launcherProcessStartTime,omitempty"`
	LauncherExecutablePath   string `json:"launcherExecutablePath,omitempty"`
	LauncherProfile          string `json:"launcherProfile,omitempty"`
	LauncherDataRoot         string `json:"launcherDataRoot,omitempty"`
	LauncherWebviewProfile   string `json:"launcherWebviewProfile,omitempty"`
	LauncherPIDNamespace     string `json:"launcherPidNamespace,omitempty"`
}

type Row struct {
	Identity
	PID      int    `json:"pid"`
	Port     int    `json:"port"`
	DataRoot string `json:"dataRoot"`
	DataDir  string `json:"dataDir"`
	Version  string `json:"version"`
}

func (r Row) Validate() error {
	if r.ID == "" {
		return errors.New("instanceinfo: registry row has no id")
	}
	if r.PID <= 0 {
		return fmt.Errorf("instanceinfo: registry row %q has invalid pid %d", r.ID, r.PID)
	}
	if err := r.Identity.Validate(r.DataRoot, r.DataDir); err != nil {
		return err
	}
	if r.IdentityVersion == 0 {
		return nil
	}
	if r.BootNonce == "" {
		return fmt.Errorf("instanceinfo: registry row %q has no boot nonce", r.ID)
	}
	return nil
}

type Instance struct {
	Row
	Path  string `json:"path"`
	Stale bool   `json:"stale"`
}

func ID(dataRoot string) string {
	sum := sha256.Sum256([]byte(canonical(dataRoot)))
	return hex.EncodeToString(sum[:])[:idHexLen]
}

// CanonicalPath resolves the deepest existing ancestor and appends any
// missing suffix. That keeps fresh roots below symlinked parents stable.
func CanonicalPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("instanceinfo: empty data root")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve data root %q: %w", path, err)
	}
	abs = filepath.Clean(abs)
	current := abs
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", fmt.Errorf("resolve existing ancestor %q: %w", current, resolveErr)
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect data root ancestor %q: %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs, nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func ValidatePaths(id, dataRoot, dataDir string) error {
	canonicalRoot, err := CanonicalPath(dataRoot)
	if err != nil {
		return err
	}
	if id != "" && id != ID(canonicalRoot) {
		return fmt.Errorf("instanceinfo: id %q does not match data root %q (want %q)", id, dataRoot, ID(canonicalRoot))
	}
	if dataDir != "" {
		canonicalDir, err := CanonicalPath(dataDir)
		if err != nil {
			return err
		}
		want := filepath.Join(canonicalRoot, "agent-overflow")
		if canonicalDir != want {
			return fmt.Errorf("instanceinfo: data dir %q does not belong to data root %q (want %q)", dataDir, dataRoot, want)
		}
	}
	return nil
}

func (i Identity) Validate(dataRoot, dataDir string) error {
	if i.IdentityVersion != 0 && i.IdentityVersion != IdentityVersion {
		return fmt.Errorf("instanceinfo: identity version %d unsupported (want %d)", i.IdentityVersion, IdentityVersion)
	}
	if i.IdentityVersion == IdentityVersion && i.BootNonce == "" {
		return errors.New("instanceinfo: current identity has no boot nonce")
	}
	launcherIdentityPresent := i.LauncherProcessStartTime != "" || i.LauncherExecutablePath != "" ||
		i.LauncherProfile != "" || i.LauncherDataRoot != "" || i.LauncherWebviewProfile != "" || i.LauncherPIDNamespace != ""
	if i.IdentityVersion == IdentityVersion && i.LauncherPid == 0 && launcherIdentityPresent {
		return errors.New("instanceinfo: launcher identity requires a launcher pid")
	}
	if i.IdentityVersion == IdentityVersion && i.LauncherPid > 0 {
		if i.LauncherProcessStartTime == "" || i.LauncherExecutablePath == "" || i.LauncherProfile == "" || i.LauncherDataRoot == "" || i.LauncherWebviewProfile == "" || i.LauncherPIDNamespace == "" {
			return errors.New("instanceinfo: current launcher identity is incomplete")
		}
		if _, err := strconv.ParseInt(i.LauncherProcessStartTime, 10, 64); err != nil {
			return fmt.Errorf("instanceinfo: launcher process birth marker is invalid: %w", err)
		}
		if !IsAbsolutePath(i.LauncherExecutablePath) || !IsAbsolutePath(i.LauncherWebviewProfile) {
			return errors.New("instanceinfo: launcher executable and WebView profile must be absolute paths")
		}
		if i.LauncherPIDNamespace != "windows" {
			return fmt.Errorf("instanceinfo: unsupported launcher pid namespace %q", i.LauncherPIDNamespace)
		}
		switch i.LauncherProfile {
		case string(ModeHarness), string(ModeSoak), string(ModePerf):
		default:
			return fmt.Errorf("instanceinfo: unsupported launcher profile %q", i.LauncherProfile)
		}
		launcherRoot, err := CanonicalPath(i.LauncherDataRoot)
		if err != nil {
			return fmt.Errorf("instanceinfo: resolve launcher data root: %w", err)
		}
		selectedRoot, err := CanonicalPath(dataRoot)
		if err != nil {
			return fmt.Errorf("instanceinfo: resolve selected data root: %w", err)
		}
		if launcherRoot != selectedRoot {
			return fmt.Errorf("instanceinfo: launcher data root %q does not match selected root %q", i.LauncherDataRoot, dataRoot)
		}
	}
	if i.Mode != "" && i.Mode != ModeHarness && i.Mode != ModeSoak && i.Mode != ModePerf {
		return fmt.Errorf("instanceinfo: unsupported mode %q", i.Mode)
	}
	return ValidatePaths(i.ID, dataRoot, dataDir)
}

func IsAbsolutePath(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return true
	}
	return len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}

func (i Identity) SameLifecycle(other Identity) bool {
	if i.IdentityVersion != 0 && other.IdentityVersion != 0 && i.IdentityVersion != other.IdentityVersion {
		return false
	}
	if i.ID != "" && other.ID != "" && i.ID != other.ID {
		return false
	}
	if i.Mode != "" && other.Mode != "" && i.Mode != other.Mode {
		return false
	}
	if i.Window != other.Window && i.IdentityVersion != 0 && other.IdentityVersion != 0 {
		return false
	}
	if i.Worktree != "" && other.Worktree != "" && i.Worktree != other.Worktree {
		return false
	}
	if i.StartedAt != "" && other.StartedAt != "" && i.StartedAt != other.StartedAt {
		return false
	}
	if i.BootNonce != "" || other.BootNonce != "" {
		if i.BootNonce == "" || i.BootNonce != other.BootNonce {
			return false
		}
	}
	if i.ProcessStartTime != "" || other.ProcessStartTime != "" {
		if i.ProcessStartTime == "" || i.ProcessStartTime != other.ProcessStartTime {
			return false
		}
	}
	if i.ExecutablePath != "" || other.ExecutablePath != "" {
		if i.ExecutablePath == "" || i.ExecutablePath != other.ExecutablePath {
			return false
		}
	}
	if i.LauncherPid != other.LauncherPid && (i.LauncherPid > 0 || other.LauncherPid > 0) {
		return false
	}
	launcherFields := [][2]string{{i.LauncherProcessStartTime, other.LauncherProcessStartTime}, {i.LauncherExecutablePath, other.LauncherExecutablePath}, {i.LauncherProfile, other.LauncherProfile}, {i.LauncherDataRoot, other.LauncherDataRoot}, {i.LauncherWebviewProfile, other.LauncherWebviewProfile}, {i.LauncherPIDNamespace, other.LauncherPIDNamespace}}
	for _, pair := range launcherFields {
		if pair[0] != "" || pair[1] != "" {
			if pair[0] == "" || pair[1] == "" || pair[0] != pair[1] {
				return false
			}
		}
	}
	return true
}

func canonical(path string) string {
	resolved, err := CanonicalPath(path)
	if err != nil {
		abs, absErr := filepath.Abs(path)
		if absErr != nil {
			return filepath.Clean(path)
		}
		return filepath.Clean(abs)
	}
	return resolved
}
