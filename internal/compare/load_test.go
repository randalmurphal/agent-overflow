package compare

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCapsuleAssetPathRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "asset.bin"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "attachments")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := capsuleAssetPath(root, "attachments/asset.bin"); err == nil {
		t.Fatal("accepted capsule asset through symlinked parent")
	}
}
