//go:build darwin || dragonfly || freebsd || netbsd || openbsd || solaris

package harnessclient

import (
	"fmt"
	"os/exec"
)

func processGroupListing() ([]processGroupListingEntry, error) {
	out, err := exec.Command("ps", "-axo", "pid=,pgid=").Output()
	if err != nil {
		return nil, fmt.Errorf("list process groups: %w", err)
	}
	return parseProcessGroupListing(string(out))
}
