package appidentity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeviceNameReadThroughAndValidation(t *testing.T) {
	dir := t.TempDir()
	one, two := NewDeviceName(dir), NewDeviceName(dir)
	if got, err := one.Get(); err != nil || got != HostDisplayName() {
		t.Fatalf("default=%q,%v", got, err)
	}
	if err := one.Set("  Studio ☀  "); err != nil {
		t.Fatal(err)
	}
	if got, err := two.Get(); err != nil || got != "Studio ☀" {
		t.Fatalf("fresh read=%q,%v", got, err)
	}
	for _, bad := range []string{"a\nb", "a\x00b", strings.Repeat("☀", 81), string([]byte{0xff})} {
		if err := one.Set(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
		if got, _ := two.Get(); got != "Studio ☀" {
			t.Fatalf("failed write changed name to %q", got)
		}
	}
	if err := one.Set(strings.Repeat("☀", 80)); err != nil {
		t.Fatal(err)
	}
	if err := two.Set(""); err != nil {
		t.Fatal(err)
	}
	if got, err := one.Get(); err != nil || got != HostDisplayName() {
		t.Fatalf("reset=%q,%v", got, err)
	}
}
func TestDeviceNameCorruptionAndFailedWriteAreErrors(t *testing.T) {
	dir := t.TempDir()
	n := NewDeviceName(dir)
	if err := os.WriteFile(filepath.Join(dir, "device-name.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Get(); err == nil {
		t.Fatal("corrupt metadata silently ignored")
	}
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewDeviceName(path).Set("new"); err == nil {
		t.Fatal("write failure ignored")
	}
}
