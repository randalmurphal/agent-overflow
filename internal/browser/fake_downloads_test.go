package browser

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// A download is cancellable from its first report: a cancel made while that
// report is handled ends it, the engine reports the cancel as its one end,
// and the download neither stays running nor finishes, whether it was to be
// held or to finish at once.
func TestAFakeDownloadCanceledAtItsFirstReportEnds(t *testing.T) {
	for _, flags := range [][]string{{"hold"}, nil} {
		t.Run(fmt.Sprintf("flags %v", flags), func(t *testing.T) {
			engine := newFakeEngine()
			engine.saving = downloadsAsArtifacts
			var profile *fakeProfile
			var mu sync.Mutex
			var states []string
			var cancelErr error
			canceled := false
			engine.events = engineEvents{
				DownloadStarted: func(downloadStart) downloadAdmission { return downloadAdmitted },
				DownloadProgress: func(progress downloadProgress) {
					mu.Lock()
					states = append(states, progress.State)
					first := !canceled
					canceled = true
					mu.Unlock()
					if first {
						cancelErr = profile.CancelDownload(progress.ID)
					}
				},
			}
			if err := engine.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			opened, err := engine.NewProfile(t.Context(), profileOptions{Jar: "w", DownloadDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			profile = opened.(*fakeProfile)
			defer func() { _ = profile.Dispose(context.Background()) }()
			request, _ := fakeDownloadAddress(fakeDownloadURL("f.bin", 64, flags...))
			if err := profile.download("frame", request); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if cancelErr != nil {
				t.Fatalf("the cancel made at the first report answered %v", cancelErr)
			}
			if len(states) != 2 || states[0] != downloadInProgress || states[1] != downloadCanceled {
				t.Fatalf("the download reported %q, want its progress and then its cancel", states)
			}
			profile.mu.Lock()
			running := len(profile.held)
			profile.mu.Unlock()
			if running != 0 {
				t.Fatalf("the canceled download is still running in the engine")
			}
		})
	}
}
