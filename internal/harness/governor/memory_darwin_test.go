//go:build darwin

package governor

import "testing"

func TestParseVMStatPageSizeReadsRealHeader(t *testing.T) {
	got, ok := parseVMStatPageSize("Mach Virtual Memory Statistics: (page size of 16384 bytes)")
	if !ok || got != 16384 {
		t.Fatalf("parseVMStatPageSize = (%d, %t), want (16384, true)", got, ok)
	}
}

func TestParseVMStatPageSizeRejectsMalformedHeader(t *testing.T) {
	if got, ok := parseVMStatPageSize("Mach Virtual Memory Statistics: (page size of bytes)"); ok || got != 0 {
		t.Fatalf("malformed page header = (%d, %t), want (0, false)", got, ok)
	}
}
