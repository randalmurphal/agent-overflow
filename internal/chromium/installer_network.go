package chromium

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

type chromeForTestingManifest struct {
	Channels map[string]chromeForTestingChannel `json:"channels"`
}

type chromeForTestingChannel struct {
	Version   string                                `json:"version"`
	Downloads map[string][]chromeForTestingDownload `json:"downloads"`
}

type chromeForTestingDownload struct {
	Platform string `json:"platform"`
	URL      string `json:"url"`
}

func (c chromeForTestingChannel) artifactURL(artifact Artifact, platform string) (string, bool) {
	for _, d := range c.Downloads[string(artifact)] {
		if d.Platform == platform {
			return d.URL, true
		}
	}
	return "", false
}

func (i *Installer) fetchManifest(ctx context.Context) (*chromeForTestingManifest, error) {
	// Tighten the manifest deadline regardless of HTTPClient.Timeout —
	// a stuck JSON server shouldn't hold the goroutine for the long
	// download budget.
	fetchCtx, cancel := context.WithTimeout(ctx, manifestFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, i.ManifestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", installerUserAgent)
	resp, err := i.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest status %d", resp.StatusCode)
	}
	// Bound the body before parse: a hostile mirror could otherwise
	// stream gigabytes of JSON and OOM the process.
	body := io.LimitReader(resp.Body, manifestMaxBytes+1)
	var m chromeForTestingManifest
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest decode: %w", err)
	}
	return &m, nil
}

// download streams the zip to dst with periodic progress events.
//
// We don't currently support HTTP Range resumption: a one-time ~150
// MB download from Google's CDN is reliable in practice, and a
// retry-on-failure UX is simpler to reason about than a partial
// resumption that can land mid-byte if the server's content shifted.
// The .partial suffix on dst exists so the caller can distinguish a
// completed file from an in-flight one for cleanup.
func (i *Installer) download(ctx context.Context, url, dst, version string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", installerUserAgent)
	resp, err := i.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}
	total := resp.ContentLength
	if total > zipMaxDownloadBytes {
		return fmt.Errorf("download size %d exceeds %d bytes", total, zipMaxDownloadBytes)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	// Throttled progress emission — once per 256 KB, to avoid flooding
	// the event ring on a fast download.
	const reportEvery = 256 * 1024
	var written, lastReport int64
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if written+int64(n) > zipMaxDownloadBytes {
				return fmt.Errorf("download exceeds %d bytes", zipMaxDownloadBytes)
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if written-lastReport >= reportEvery || readErr == io.EOF {
				lastReport = written
				i.emit(InstallProgress{
					Phase:      "downloading",
					Downloaded: written,
					Total:      total,
					Version:    version,
				})
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	if total > 0 && written != total {
		return fmt.Errorf("downloaded %d bytes, expected %d", written, total)
	}
	return nil
}

// unzip extracts src to dst with three defenses:
//
//   - Slip-zip: every extracted path must stay rooted under dst.
//   - Zip-bomb: per-entry and aggregate decompressed-size caps; an
//     archive that exceeds either is rejected before consuming the
//     overflowing bytes (the LimitReader is +1 to detect overflow).
//   - Symlink: zip entries with the symlink mode bit are skipped;
//     extracting a symlink would let later operations (open, chmod,
//     remove) follow a link out of dst even with the path-prefix
//     guard satisfied at extraction time.
//
// File modes from the zip have non-permission bits (setuid/setgid/
// sticky) masked off; we never need to preserve them and they invite
// privilege-escalation surface a malicious archive could exploit.
