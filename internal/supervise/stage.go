package supervise

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/selfupdate"
)

// StageBinary copies one executable into its immutable version directory.
//
// The copy runs through selfupdate.StageCopy, which is already the tree's
// verified-temp-then-rename staging primitive: it hashes the bytes as they are
// written and renames only on a match, so a version directory never holds a
// partial or a torn file. The digest is computed from the source here rather
// than supplied, because at this layer there is no publisher to compare
// against — what is being defended is the copy, not the provenance. Provenance
// (a release feed, its checksum, its signature) belongs to the wave that
// downloads, and it verifies before it ever calls this.
//
// A version directory that already exists is REPLACED rather than refused,
// because the only caller that can hit one is an operator restaging the same
// version after a partial install; a version names one build, so the bytes are
// the same bytes.
func StageBinary(layout Layout, version, source string) error {
	dir, err := layout.VersionDir(version)
	if err != nil {
		return err
	}
	if source == "" {
		return errors.New("supervise: the binary to stage is required")
	}
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("supervise: stat %s: %w", source, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("supervise: %s is not a regular file", source)
	}
	digest, err := fileDigest(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("supervise: create %s: %w", dir, err)
	}
	if _, err := selfupdate.StageCopy(source, dir, BinaryName, digest); err != nil {
		return fmt.Errorf("supervise: stage version %s: %w", version, err)
	}
	if err := atomicfile.SyncDir(dir); err != nil {
		return err
	}
	return atomicfile.SyncDir(layout.VersionsDir())
}

func fileDigest(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("supervise: open %s: %w", path, err)
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return nil, fmt.Errorf("supervise: read %s: %w", path, err)
	}
	return hasher.Sum(nil), nil
}
