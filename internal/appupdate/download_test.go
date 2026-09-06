package appupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

type streamingRelease struct {
	stubProvider
	started *bool
	chunks  []string
}

func (p streamingRelease) Download(_ context.Context, _ *updater.Release, dst io.Writer, _ func(int64, int64)) error {
	*p.started = true
	for _, chunk := range p.chunks {
		if _, err := io.WriteString(dst, chunk); err != nil {
			return err
		}
	}
	return nil
}

func TestReleaseDownloadBoundsReportedAndActualBytes(t *testing.T) {
	for _, test := range []struct {
		name              string
		reported          int64
		chunks            []string
		want              string
		started, tooLarge bool
	}{
		{"reported size refused before download", 7, []string{"abc"}, "", false, true},
		{"unknown size fits exactly", 0, []string{"abc", "def"}, "abcdef", true, false},
		{"unknown size exceeds limit", 0, []string{"abc", "def", "g"}, "abcdef", true, true},
		{"understated size exceeds limit", 1, []string{"abcdefg"}, "", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var dst bytes.Buffer
			started := false
			provider := streamingRelease{started: &started, chunks: test.chunks}
			err := downloadBoundedArtifact(context.Background(), provider, &updater.Release{Artifact: updater.Artifact{Size: test.reported}}, &dst, nil, 6)
			if errors.Is(err, errDownloadTooLarge) != test.tooLarge || started != test.started || dst.String() != test.want {
				t.Fatalf("download = %q, started %v, error %v", dst.String(), started, err)
			}
		})
	}
}
