package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/transferfiles"
	"github.com/klauspost/compress/zstd"
)

const transferRootID = "63c2793a-d680-4481-8bf3-6c520e61f9b4"
const transferBaseID = "0f8ac89f-8b2f-4e5d-9c89-851dbd27aaf3"

func transferRollout(t *testing.T, home, filenameID, nativeID, base, mode string, compressed bool) string {
	t.Helper()
	file := filepath.Join(home, "sessions", "2026", "09", "05", "rollout-2026-09-05T12-00-00-"+filenameID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"id": nativeID, "cwd": "/source/workspace", "history_mode": mode, "cli_version": "0.153.4"}
	if base != "" {
		payload["history_base"] = map[string]any{"thread_id": base, "end_ordinal_exclusive": 1, "end_byte_offset": 100}
	}
	data, err := json.Marshal(map[string]any{"type": "session_meta", "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if compressed {
		encoder, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		data = encoder.EncodeAll(data, nil)
		encoder.Close()
		file += ".zst"
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestTransferCollectsRevertedHistoryAndCompressedPrefixes(t *testing.T) {
	home := t.TempDir()
	// A revert's filename id differs from its stable native session id.
	current := "91ee6033-ff25-4600-846e-c75e4ec184d4"
	head := transferRollout(t, home, current, transferRootID, transferBaseID, HistoryModePaginated, false)
	base := transferRollout(t, home, transferBaseID, transferRootID, "", HistoryModePaginated, true)
	transferRollout(t, home, "737f9d87-9d77-43cd-a680-c3b5c06b9d61", "737f9d87-9d77-43cd-a680-c3b5c06b9d61", "", HistoryModeLegacy, false)
	sources, err := TransferFiles(context.Background(), home, []TransferReference{{transferRootID, head}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || !strings.HasSuffix(sources[0].Name, filepath.Base(base)) || !strings.HasSuffix(sources[1].Name, filepath.Base(head)) {
		t.Fatalf("wrong dependency closure: %+v", sources)
	}
	archive := filepath.Join(t.TempDir(), "native.tar")
	digest, err := transferfiles.Create(context.Background(), archive, sources)
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	stage := filepath.Join(t.TempDir(), "stage")
	if _, err := transferfiles.Extract(context.Background(), in, digest, stage); err != nil {
		t.Fatal(err)
	}
	destHome := filepath.Join(stage, "native", "codex")
	destHead := filepath.Join(destHome, "sessions", filepath.FromSlash(sources[1].Path))
	destSources, err := TransferFiles(context.Background(), destHome, []TransferReference{{transferRootID, destHead}})
	if err != nil || len(destSources) != 2 {
		t.Fatalf("moved dependency closure: %+v %v", destSources, err)
	}
}

func TestTransferRefusesMissingCyclicOrFutureHistory(t *testing.T) {
	for _, test := range []string{"missing", "cycle", "future", "wrong native identity", "outside home"} {
		t.Run(test, func(t *testing.T) {
			home := t.TempDir()
			base, mode, id := transferBaseID, HistoryModePaginated, transferRootID
			if test == "future" {
				base, mode = "", "future-format"
			}
			if test == "wrong native identity" {
				base, id = "", transferBaseID
			}
			head := transferRollout(t, home, transferRootID, id, base, mode, false)
			if test == "cycle" {
				transferRollout(t, home, transferBaseID, transferRootID, transferRootID, HistoryModePaginated, false)
			}
			if test == "outside home" {
				head = transferRollout(t, t.TempDir(), transferRootID, transferRootID, "", HistoryModeLegacy, false)
			}
			if _, err := TransferFiles(context.Background(), home, []TransferReference{{transferRootID, head}}); err == nil {
				t.Fatal("accepted invalid history")
			}
		})
	}
}

func TestTransferPrefersPlainRolloutAndFindsCompressedHead(t *testing.T) {
	home := t.TempDir()
	compressed := transferRollout(t, home, transferRootID, transferRootID, "", HistoryModeLegacy, true)
	refs := []TransferReference{{transferRootID, strings.TrimSuffix(compressed, ".zst")}}
	sources, err := TransferFiles(context.Background(), home, refs)
	if err != nil || len(sources) != 1 || !strings.HasSuffix(sources[0].Name, ".zst") {
		t.Fatalf("compressed head: %+v %v", sources, err)
	}
	plain := transferRollout(t, home, transferRootID, transferRootID, "", HistoryModeLegacy, false)
	sources, err = TransferFiles(context.Background(), home, []TransferReference{{transferRootID, compressed}})
	if err != nil || len(sources) != 1 || !strings.HasSuffix(sources[0].Name, filepath.Base(plain)) {
		t.Fatalf("plain precedence: %+v %v", sources, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := TransferFiles(ctx, home, refs); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err := TransferFiles(context.Background(), "", refs); err == nil {
		t.Fatal("defaulted provider home")
	}
}

func TestTransferAcceptsProviderCanonicalHomeWithoutReadingOutside(t *testing.T) {
	home := t.TempDir()
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	file := transferRollout(t, canonical, transferRootID, transferRootID, "", HistoryModeLegacy, false)
	alias := filepath.Join(t.TempDir(), "provider-home")
	if err := os.Symlink(canonical, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation needs Windows developer mode")
		}
		t.Fatal(err)
	}
	sources, err := TransferFiles(context.Background(), alias, []TransferReference{{transferRootID, file}})
	if err != nil || len(sources) != 1 {
		t.Fatalf("canonical provider home rejected: %+v %v", sources, err)
	}
	outside := transferRollout(t, t.TempDir(), transferRootID, transferRootID, "", HistoryModeLegacy, false)
	if _, err := TransferFiles(context.Background(), alias, []TransferReference{{transferRootID, outside}}); !errors.Is(err, ErrOutsideCodexHome) {
		t.Fatalf("home alias bypassed containment: %v", err)
	}
}
