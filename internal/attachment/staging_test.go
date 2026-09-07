package attachment

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestStagedUploadPublishesOnceOrAbortsWithoutThreadFiles(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit", false: "abort"}[commit], func(t *testing.T) {
			attachments, meta := newTestStores(t)
			seedThread(t, meta, "t1")
			staged, err := attachments.StageUpload("t1", "report.txt", "text/plain", 5, bytes.NewBufferString("hello"), 0)
			if err != nil {
				t.Fatal(err)
			}
			defer staged.Abort()
			if _, err := os.Stat(filepath.Join(attachments.Root(), "t1")); !os.IsNotExist(err) {
				t.Fatalf("staging created thread path: %v", err)
			}
			records, err := attachments.List("t1")
			if err != nil || len(records) != 0 {
				t.Fatalf("staging published metadata: %+v %v", records, err)
			}
			temporary := staged.temporary
			if commit {
				row, err := staged.Commit()
				if err != nil {
					t.Fatal(err)
				}
				_, body, err := attachments.ReadBytes(row.ID)
				if err != nil || string(body) != "hello" {
					t.Fatalf("committed bytes: %q %v", body, err)
				}
			} else {
				staged.Abort()
			}
			if _, err := staged.Commit(); err == nil {
				t.Fatal("consumed stage committed again")
			}
			if _, err := os.Stat(temporary); !os.IsNotExist(err) {
				t.Fatalf("stage remained after completion: %v", err)
			}
		})
	}
}

func TestNewStorePrunesOnlyAbandonedUploadStages(t *testing.T) {
	attachments, meta := newTestStores(t)
	abandoned := filepath.Join(attachments.Root(), ".upload-"+uuid.NewString()+".tmp")
	untouched := filepath.Join(attachments.Root(), ".upload-not-an-id.tmp")
	for _, path := range []string{abandoned, untouched} {
		if err := os.WriteFile(path, []byte("unpublished"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewStore(Config{RootDir: attachments.Root()}, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned stage survived startup: %v", err)
	}
	if _, err := os.Stat(untouched); err != nil {
		t.Fatalf("startup removed unrelated file: %v", err)
	}
}
