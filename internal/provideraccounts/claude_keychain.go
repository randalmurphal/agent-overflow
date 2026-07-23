package provideraccounts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

var ErrCredentialMissing = errors.New("provideraccounts: native credential not found")

func readClaudeKeychainCredential(configHome string, active bool) ([]byte, error) {
	service, username, err := claudeKeychainIdentity(configHome, active)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("security", "find-generic-password", "-a", username, "-w", "-s", service)
	output, err := cmd.Output()
	if err != nil {
		return nil, ErrCredentialMissing
	}
	data := []byte(strings.TrimSpace(string(output)))
	if len(data) == 0 {
		return nil, ErrCredentialMissing
	}
	if len(data) > maxCredentialBytes {
		return nil, errors.New("provideraccounts: Claude Keychain credential exceeds size limit")
	}
	return data, nil
}

func writeClaudeKeychainCredential(configHome string, active bool, data []byte) error {
	if len(data) == 0 || len(data) > maxCredentialBytes {
		return errors.New("provideraccounts: invalid Claude Keychain credential size")
	}
	service, username, err := claudeKeychainIdentity(configHome, active)
	if err != nil {
		return err
	}
	hexValue := hex.EncodeToString(data)
	command := fmt.Sprintf(
		"add-generic-password -U -a %s -s %s -X %s\n",
		strconv.Quote(username),
		strconv.Quote(service),
		strconv.Quote(hexValue),
	)
	var cmd *exec.Cmd
	if len(command) <= 4032 {
		cmd = exec.Command("security", "-i")
		cmd.Stdin = strings.NewReader(command)
	} else {
		// Matches Claude Code's native fallback for Keychain payloads larger
		// than security(1)'s fixed interactive-input buffer. The value is
		// hex-encoded, and provider output is never surfaced.
		cmd = exec.Command(
			"security", "add-generic-password", "-U",
			"-a", username, "-s", service, "-X", hexValue,
		)
	}
	if err := cmd.Run(); err != nil {
		return errors.New("provideraccounts: update Claude Keychain credential")
	}
	return nil
}

func deleteClaudeKeychainCredential(configHome string, active bool) error {
	service, username, err := claudeKeychainIdentity(configHome, active)
	if err != nil {
		return err
	}
	cmd := exec.Command("security", "delete-generic-password", "-a", username, "-s", service)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
			return nil
		}
		return errors.New("provideraccounts: remove Claude Keychain credential")
	}
	return nil
}

func claudeKeychainIdentity(configHome string, active bool) (string, string, error) {
	username := strings.TrimSpace(os.Getenv("USER"))
	if username == "" {
		current, err := user.Current()
		if err != nil {
			return "", "", fmt.Errorf("provideraccounts: resolve Keychain user: %w", err)
		}
		username = current.Username
	}
	service := "Claude Code-credentials"
	if !active {
		hash := sha256.Sum256([]byte(filepath.Clean(configHome)))
		service += "-" + hex.EncodeToString(hash[:])[:8]
	}
	return service, username, nil
}
