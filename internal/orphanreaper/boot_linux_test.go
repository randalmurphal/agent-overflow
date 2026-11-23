//go:build linux

package orphanreaper

import (
	"os"
	"strings"
	"testing"
)

// On Linux a start time counts from boot, so an identity carries the
// kernel's boot id.
func TestIdentityCarriesTheKernelBootID(t *testing.T) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	id, err := Identify(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(data)); id.Boot != want || want == "" {
		t.Fatalf("identity %+v, want boot %q", id, want)
	}
}
