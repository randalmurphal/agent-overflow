package rollout

import (
	"context"
	"testing"

	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

func TestTransferVersionFollowsHistoryFormatIncludingChildren(t *testing.T) {
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
		minimum, err := TransferMinimumVersion(context.Background(), files)
		switch mode {
		case HistoryModeLegacy:
			if err != nil || minimum != "" {
				t.Fatal("legacy inherited producer version", minimum, err)
			}
		case HistoryModePaginated:
			if err != nil || minimum != "0.148.0" {
				t.Fatal("paginated child lost format floor", minimum, err)
			}
		default:
			if err == nil {
				t.Fatal("unknown format accepted after known paginated child")
			}
		}
	}
}
