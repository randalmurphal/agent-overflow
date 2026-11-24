package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A download is cancellable from its start: a cancel made while the start
// is admitted (an immediate cancel of the row the admission lists) is
// taken, the download reports that cancel as its one end after its start,
// with no bytes, and it neither writes, stays running nor finishes.
func TestAFakeDownloadCanceledDuringItsAdmissionEnds(t *testing.T) {
	for _, flags := range [][]string{{"hold"}, nil} {
		t.Run(fmt.Sprintf("flags %v", flags), func(t *testing.T) {
			engine := newFakeEngine()
			engine.saving = downloadsAsArtifacts
			var profile *fakeProfile
			var states []downloadState
			var received []float64
			var cancelErr error
			var id downloadID
			engine.events = engineEvents{
				DownloadStarted: func(start downloadStart) downloadAdmission {
					id = start.ID
					cancelErr = profile.CancelDownload(start.ID)
					return downloadAdmitted
				},
				DownloadProgress: func(progress downloadProgress) {
					states = append(states, progress.State)
					received = append(received, progress.Received)
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
			if cancelErr != nil {
				t.Fatalf("the cancel made during the admission answered %v", cancelErr)
			}
			if len(states) != 1 || states[0] != downloadCanceled || received[0] != 0 {
				t.Fatalf("the download reported %q with %v bytes, want its cancel alone with none", states, received)
			}
			if _, err := os.Lstat(filepath.Join(profile.downloadDir, string(id))); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the download canceled at its start wrote its file: %v", err)
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
			var states []downloadState
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

// finishHeld finishes a held download the way the native engines do: the
// file is complete, the engine's record of it is released, and only then is
// the finish reported.
func (p *fakeProfile) finishHeld(id downloadID) error {
	p.mu.Lock()
	held, ok := p.held[id]
	ok = ok && held.phase == fakeHeld
	if ok {
		held.phase = fakeEnding
		p.held[id] = held
	}
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("browser: %s is not a held fake download", id)
	}
	defer p.dropDownload(id)
	if err := os.WriteFile(held.file, make([]byte, held.total), 0o600); err != nil {
		return err
	}
	if released := p.engine.downloadReleased; released != nil {
		released(id)
	}
	p.reportFinish(id, held.total, held.file)
	return nil
}
