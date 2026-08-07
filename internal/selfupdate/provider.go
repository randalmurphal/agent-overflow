package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// ProviderName identifies StagedFileProvider in updater progress/error payloads.
const ProviderName = "staged-file"

// copyChunkSize is how much StagedFileProvider.Download moves between progress
// callbacks. Large enough that the copy is not syscall-bound, small enough that
// a tens-of-MB artifact still reports progress smoothly.
const copyChunkSize = 128 << 10

// StagedFileProvider adapts one already-staged local artifact to
// updater.Provider, so the launcher can drive the stock Updater — its streaming
// digest verification, staging, swap helper and relaunch — against a file that
// was downloaded by another process instead of fetched from a release feed.
//
// Check deliberately applies no newer-than-current gating: reaching this
// provider means the user already chose this exact build, rollbacks included.
type StagedFileProvider struct {
	path    string
	version string
	digest  []byte
}

// NewStagedFileProvider describes the staged artifact at path as release
// version, to be verified against the raw SHA-256 digest.
func NewStagedFileProvider(path, version string, digest []byte) *StagedFileProvider {
	return &StagedFileProvider{path: path, version: version, digest: digest}
}

// Name implements updater.Provider.
func (p *StagedFileProvider) Name() string { return ProviderName }

// Check implements updater.Provider. It validates what it was constructed with
// and stats the staged file, so a missing or unverifiable artifact fails here —
// before the Updater transitions into a download it cannot complete.
func (p *StagedFileProvider) Check(_ context.Context, req updater.CheckRequest) (*updater.Release, error) {
	if err := validateVersion(p.version); err != nil {
		return nil, err
	}
	if err := validateDigest(p.digest); err != nil {
		return nil, err
	}
	info, err := os.Stat(p.path)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: stat staged artifact: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("selfupdate: staged artifact %s is not a regular file", p.path)
	}
	return &updater.Release{
		Version: p.version,
		Artifact: updater.Artifact{
			Filename: filepath.Base(p.path),
			Size:     info.Size(),
			Platform: req.Platform,
			Arch:     req.Arch,
		},
		Verification: &updater.Verification{DigestAlgo: "sha256", Digest: p.digest},
	}, nil
}

// Download implements updater.Provider by streaming the staged file to dst. The
// Updater hashes the same bytes as they pass and compares them against the
// Verification block above, so a file that was tampered with between staging and
// install fails verification exactly like a corrupted download would.
func (p *StagedFileProvider) Download(ctx context.Context, _ *updater.Release, dst io.Writer, onProgress func(written, total int64)) error {
	f, err := os.Open(p.path)
	if err != nil {
		return fmt.Errorf("selfupdate: open staged artifact: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("selfupdate: stat staged artifact: %w", err)
	}
	total := info.Size()

	var written int64
	buf := make([]byte, copyChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return fmt.Errorf("selfupdate: write staged artifact: %w", err)
			}
			written += int64(n)
			if onProgress != nil {
				onProgress(written, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("selfupdate: read staged artifact: %w", readErr)
		}
	}
	return nil
}
