package threadtransfer

import (
	"encoding/json"
	"errors"
	"io"
	"os"

	"agent-overflow/internal/atomicfile"
)

// Recovery recipes contain metadata, never file contents. Refuse an unusually
// large recipe before source retirement, keeping four concurrent jobs bounded.
const maxDestinationPlanBytes int64 = 16 << 20

func writeDestinationJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if int64(len(data)) > maxDestinationPlanBytes {
		return errors.New("transfer: preparation recipe exceeds its limit")
	}
	return atomicfile.Write(path, data)
}

func readDestinationJSON(path string, limit int64, value any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("transfer: preparation recipe exceeds its limit")
	}
	return json.Unmarshal(data, value)
}
