package transferfiles

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUploadResumesAfterLostReplyAndUncommittedCrashTail(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "upload")
	body := bytes.Repeat([]byte("x"), 2048)
	digest := checksum(body)
	if _, err := BeginUpload(directory, digest, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	// A previous process died mid-chunk. Its single scratch file is reused.
	if err := os.WriteFile(filepath.Join(directory, ".chunk.part"), bytes.Repeat([]byte("old"), 2048), 0600); err != nil {
		t.Fatal(err)
	}
	chunk := body[:1024]
	for range 2 {
		progress, err := ReceiveChunk(context.Background(), directory, 0, 1024, checksum(chunk), bytes.NewReader(chunk))
		if err != nil || progress.Received != 1024 {
			t.Fatalf("lost reply retry: %+v %v", progress, err)
		}
	}
	// Simulate death after append but before checkpoint. The next request must
	// discard this tail instead of sending those unacknowledged bytes onward.
	file, err := os.OpenFile(filepath.Join(directory, uploadFileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("uncommitted")); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	progress, err := BeginUpload(directory, digest, int64(len(body)))
	if err != nil || progress.Received != 1024 {
		t.Fatalf("restart: %+v %v", progress, err)
	}
	progress, err = ReceiveChunk(context.Background(), directory, 1024, 1024, checksum(chunk), bytes.NewReader(chunk))
	if err != nil || progress.Received != 2048 {
		t.Fatalf("resume: %+v %v", progress, err)
	}
	if _, err := os.Stat(filepath.Join(directory, ".chunk.part")); !os.IsNotExist(err) {
		t.Fatal("retained crash chunk", err)
	}
	archive, _, err := UploadedArchive(directory)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(archive)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("corrupt resumed archive: %v", err)
	}
}

func TestUploadRefusesGapsCorruptChunksAndChangedIdentity(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "upload")
	body := bytes.Repeat([]byte("x"), 2048)
	if _, err := BeginUpload(directory, checksum(body), 2048); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginUpload(directory, checksum([]byte("changed")), 2048); err == nil {
		t.Fatal("rebound archive identity")
	}
	for _, test := range []struct {
		offset, size int64
		digest       string
		body         []byte
	}{
		{1, 1, checksum(body[:1]), body[:1]},
		{0, 1024, checksum([]byte("wrong")), body[:1024]},
		{0, 1024, checksum(body[:1024]), body[:512]},
		{0, 512, checksum(body[:512]), body[:1024]},
	} {
		if _, err := ReceiveChunk(context.Background(), directory, test.offset, test.size, test.digest, bytes.NewReader(test.body)); err == nil {
			t.Fatal("accepted invalid chunk")
		}
		progress, err := ReadUpload(directory)
		if err != nil || progress.Received != 0 {
			t.Fatalf("failed chunk advanced progress: %+v %v", progress, err)
		}
	}
	if _, _, err := UploadedArchive(directory); err == nil {
		t.Fatal("exposed incomplete upload")
	}
	if _, err := ReceiveChunk(context.Background(), directory, 0, 1024, checksum(body[:1024]), bytes.NewReader(body[:1024])); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Repeat([]byte("y"), 1024)
	if _, err := ReceiveChunk(context.Background(), directory, 0, 1024, checksum(changed), bytes.NewReader(changed)); err == nil {
		t.Fatal("accepted changed retry bytes")
	}
	if err := os.Truncate(filepath.Join(directory, uploadFileName), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := ReceiveChunk(context.Background(), directory, 1024, 1024, checksum(body[:1024]), bytes.NewReader(body[:1024])); err == nil {
		t.Fatal("ignored lost acknowledged bytes")
	}
}
