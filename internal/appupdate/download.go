package appupdate

import (
	"context"
	"errors"
	"io"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// Applies to both the desktop updater and supervised downloads, including
// responses without an honest Content-Length. Bound disk use before hashing
// or archive expansion; a timeout alone is not a byte limit.
const maxDownloadBytes int64 = 2 << 30

var errDownloadTooLarge = errors.New("updater: release artifact exceeds the download size limit")

func downloadBoundedArtifact(ctx context.Context, provider updater.Provider, release *updater.Release, dst io.Writer, progress func(int64, int64), limit int64) error {
	if release == nil {
		return errors.New("updater: no release to download")
	}
	if release.Artifact.Size > limit {
		return errDownloadTooLarge
	}
	return provider.Download(ctx, release, &artifactWriter{dst: dst, remaining: limit}, progress)
}

type artifactWriter struct {
	dst       io.Writer
	remaining int64
}

func (w *artifactWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errDownloadTooLarge
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	return n, err
}
