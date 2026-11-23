//go:build linux

package orphanreaper

import (
	"fmt"
	"os"
	"strings"
)

// bootIDPath holds a random id the kernel picks at boot. Containers and WSL
// read the same file.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

func bootID() (string, error) {
	data, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("read boot id: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("read boot id: %s is empty", bootIDPath)
	}
	return id, nil
}
