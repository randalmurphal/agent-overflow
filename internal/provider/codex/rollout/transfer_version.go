package rollout

import (
	"context"
	"fmt"

	"agent-overflow/internal/transferfiles"
)

// ValidateTransferHistory refuses a transfer whose native files record a
// history mode this build does not know. Every Codex AO runs reads both
// legacy and paginated history, so a known mode needs nothing more.
func ValidateTransferHistory(ctx context.Context, files []transferfiles.Source) error {
	if len(files) > transferfiles.MaxFiles {
		return fmt.Errorf("codex transfer: too many native files")
	}
	for _, file := range files {
		meta, err := readTransferMeta(ctx, file, "")
		if err != nil {
			return err
		}
		switch meta.HistoryMode {
		case "", HistoryModeLegacy, HistoryModePaginated:
		default:
			return fmt.Errorf("codex transfer: unsupported history mode %q", meta.HistoryMode)
		}
	}
	return nil
}
