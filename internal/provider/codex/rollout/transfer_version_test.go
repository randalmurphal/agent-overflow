package rollout

import (
	"context"
	"testing"

	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

func TestTransferHistoryRefusesUnknownFormatIncludingChildren(t *testing.T) {
	home := t.TempDir()
	var files []transferfiles.Source
	for _, mode := range []string{HistoryModeLegacy, HistoryModePaginated, "unknown-future-format"} {
		id := uuid.NewString()
		meta := nativeTransferMeta(id, id, "")
		payload := meta["payload"].(map[string]any)
		payload["history_mode"], payload["cli_version"] = mode, "99.0.0"
		ref := nativeTransferFixture(t, home, id, meta)
		file, err := transferSource(home, ref.Path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
		err = ValidateTransferHistory(context.Background(), files)
		switch mode {
		case HistoryModeLegacy, HistoryModePaginated:
			if err != nil {
				t.Fatal(mode, "history refused", err)
			}
		default:
			if err == nil {
				t.Fatal("unknown format accepted after known children")
			}
		}
	}
}
