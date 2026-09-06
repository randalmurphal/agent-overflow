package transferfiles

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveRoundTrip(t *testing.T) {
	root := t.TempDir()
	body := bytes.Repeat([]byte("native history\n"), 100_000)
	if err := os.WriteFile(filepath.Join(root, "history.jsonl"), body, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "bundle.tar")
	name := "native/" + strings.Repeat("long/", 70) + "history.jsonl"
	digest, err := Create(context.Background(), archive, []Source{{root, "history.jsonl", name}})
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	dest := filepath.Join(t.TempDir(), "staged")
	files, err := Extract(context.Background(), in, digest, dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != name || !files[0].Executable || files[0].SHA256 != checksum(body) {
		t.Fatalf("manifest: %+v", files)
	}
	got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(name)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("content differs: %v", err)
	}
	if _, err := Create(context.Background(), archive, nil); !errors.Is(err, os.ErrExist) {
		t.Fatalf("snapshot overwrite: %v", err)
	}
}

func TestArchiveRejectsUntrustedMembersAndDiscardsStaging(t *testing.T) {
	regular := func(name string) *tar.Header {
		return &tar.Header{Name: name, Size: 1, Mode: 0o600, Typeflag: tar.TypeReg}
	}
	cases := map[string][]*tar.Header{
		"traversal": {regular("../outside")}, "absolute": {regular("/outside")},
		"windows drive": {regular("C:/outside")}, "windows separator": {regular(`native\..\outside`)},
		"alternate stream": {regular("native/file:secret")}, "device": {regular("native/con.txt")},
		"duplicate": {regular("native/a"), regular("native/a")}, "case alias": {regular("native/a"), regular("native/A")},
		"directory alias": {regular("native/a."), regular("native/a")},
		"symlink":         {{Name: "native/link", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}},
		"hardlink":        {{Name: "native/link", Typeflag: tar.TypeLink, Linkname: "native/file"}},
		"fifo":            {{Name: "native/fifo", Typeflag: tar.TypeFifo}},
		"pax metadata":    {{Name: "native/file", Typeflag: tar.TypeReg, PAXRecords: map[string]string{"vendor.extra": "value"}}},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			for _, h := range headers {
				if err := tw.WriteHeader(h); err != nil {
					t.Fatal(err)
				}
				if h.Size != 0 {
					_, _ = tw.Write([]byte("x"))
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "staged")
			if _, err := Extract(context.Background(), bytes.NewReader(buf.Bytes()), checksum(buf.Bytes()), dest); err == nil {
				t.Fatal("accepted unsafe archive")
			}
			if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("left failed staging: %v", err)
			}
		})
	}
}

func TestArchiveRefusesCorruptionTruncationAndCancellation(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "native/file", Size: 1, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	valid := append([]byte(nil), buf.Bytes()...)
	for name, data := range map[string][]byte{"empty": {}, "cut content": valid[:512], "no end marker": valid[:1024], "one end block": valid[:1536], "garbage suffix": append(append([]byte(nil), valid...), byte('x'))} {
		t.Run(name, func(t *testing.T) {
			if _, err := Extract(context.Background(), bytes.NewReader(data), checksum(data), filepath.Join(t.TempDir(), "stage")); err == nil {
				t.Fatal("accepted malformed archive")
			}
		})
	}
	if _, err := Extract(context.Background(), bytes.NewReader(valid), checksum([]byte("wrong")), filepath.Join(t.TempDir(), "stage")); err == nil {
		t.Fatal("accepted wrong digest")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dest := filepath.Join(t.TempDir(), "stage")
	if _, err := Extract(ctx, bytes.NewReader(valid), checksum(valid), dest); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("left canceled staging: %v", err)
	}
}

func TestArchiveRefusesSymlinkSourcesAndExcessSize(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "link")); err == nil {
		if _, err := Create(context.Background(), filepath.Join(t.TempDir(), "bundle"), []Source{{root, "link", "native/file"}}); err == nil {
			t.Fatal("copied symbolic link")
		}
	}
	f, err := os.Create(filepath.Join(root, "oversized"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxFileBytes + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	destination := filepath.Join(t.TempDir(), "bundle")
	if _, err := Create(context.Background(), destination, []Source{{root, "oversized", "native/file"}}); err == nil {
		t.Fatal("accepted oversized file")
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("left failed archive: %v", err)
	}
}

func TestArchiveEmptySnapshotAndPortableNames(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "empty")
	digest, err := Create(context.Background(), destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	files, err := Extract(context.Background(), in, digest, filepath.Join(t.TempDir(), "staged"))
	if err != nil || len(files) != 0 {
		t.Fatalf("empty snapshot: %v %v", files, err)
	}
	for _, name := range []string{"", ".", "a/../b", "a//b", "a\x00b", "a/new\nline", "COM1", "a/LPT9.txt", "a/end ", "a/file?"} {
		if ValidName(name) {
			t.Errorf("accepted %q", name)
		}
	}
	for _, name := range []string{"native/history.jsonl", "workspace/a b", "native/unicode-λ", "a/com10.txt"} {
		if !ValidName(name) {
			t.Errorf("refused %q", name)
		}
	}
}

func checksum(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
