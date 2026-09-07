package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
)

type pausedAttachmentBody struct {
	reader           io.Reader
	started, release chan struct{}
	once             sync.Once
}

func (b *pausedAttachmentBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started); <-b.release })
	return b.reader.Read(p)
}
func startPausedAttachment(t *testing.T, app *App, ctx context.Context) (func(), <-chan error) {
	t.Helper()
	body := &pausedAttachmentBody{reader: bytes.NewReader(pngSignature()), started: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(body.release) }) }
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, err := app.storeAttachment(ctx, "thr-a", "paused.png", "image/png", 8, body)
		done <- err
	}()
	t.Cleanup(func() { release(); <-finished })
	select {
	case <-body.started:
	case err := <-done:
		t.Fatalf("upload refused before body: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("upload never read body")
	}
	return release, done
}

func TestStalledUploadDoesNotBlockDraftOrQueueAdmission(t *testing.T) {
	app := newAttachmentTestApp(t)
	app.ensureTriageRouter()
	release, done := startPausedAttachment(t, app, context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := app.SaveDraft(ctx, "thr-a", "during upload", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := app.RegisterQueueItem(ctx, "thr-a", "queued during upload", SendMessageOptions{SendID: "during-upload"}); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rows, err := app.attachments.List("thr-a")
	if err != nil || len(rows) != 1 {
		t.Fatalf("upload commit: %+v %v", rows, err)
	}
}

func TestUploadRevalidatesOwnerBeforePublication(t *testing.T) {
	for _, change := range []string{"delete", "empty-draft-delete", "transfer", "cancel"} {
		t.Run(change, func(t *testing.T) {
			app := newAttachmentTestApp(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release, done := startPausedAttachment(t, app, ctx)
			switch change {
			case "delete":
				if err := app.DeleteThread("thr-a"); err != nil {
					t.Fatal(err)
				}
			case "empty-draft-delete":
				if removed, err := app.DeleteEmptyDraftThread("thr-a"); err != nil || !removed {
					t.Fatalf("delete empty draft: %v %v", removed, err)
				}
			case "transfer":
				unlock, err := app.threadApplication().LockMutable(ctx, "thr-a")
				if err != nil {
					t.Fatal(err)
				}
				_, err = app.store.CreateThreadTransfer(store.ThreadTransfer{ID: entityid.New(), ThreadID: "thr-a", PeerBackendID: entityid.New(), Kind: "move", Direction: "outgoing", ActivationHash: strings.Repeat("a", 64), PrivateState: json.RawMessage(`{}`)})
				unlock()
				if err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			}
			release()
			if err := <-done; err == nil {
				t.Fatal("changed owner published staged upload")
			}
			rows, err := app.attachments.List("thr-a")
			if err != nil || len(rows) != 0 {
				t.Fatalf("refused upload metadata: %+v %v", rows, err)
			}
			err = filepath.WalkDir(app.attachments.Root(), func(path string, entry fs.DirEntry, err error) error {
				if err == nil && !entry.IsDir() {
					t.Errorf("refused upload left file %s", path)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

type unreadAttachmentBody struct{ t *testing.T }

func (r unreadAttachmentBody) Read([]byte) (int, error) {
	r.t.Error("invalid owner consumed upload bytes")
	return 0, io.EOF
}
func TestUploadRefusesMissingOrTransferringOwnerBeforeReading(t *testing.T) {
	for _, missing := range []bool{false, true} {
		app := newAttachmentTestApp(t)
		if missing {
			if err := app.DeleteThread("thr-a"); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := app.store.CreateThreadTransfer(store.ThreadTransfer{ID: entityid.New(), ThreadID: "thr-a", PeerBackendID: entityid.New(), Kind: "move", Direction: "outgoing", ActivationHash: strings.Repeat("a", 64), PrivateState: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := app.storeAttachment(context.Background(), "thr-a", "refused.png", "image/png", 8, unreadAttachmentBody{t}); err == nil {
			t.Fatal("invalid owner admitted upload")
		}
	}
}
