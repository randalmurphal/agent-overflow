package main

import (
	"fmt"
	"io"
	"os"
)

func readRunPlan(path string) ([]byte, error) {
	const maxRunPlanBytes = 4 << 20
	read := func(r io.Reader, source string) ([]byte, error) {
		data, err := io.ReadAll(io.LimitReader(r, maxRunPlanBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read run plan %s: %w", source, err)
		}
		if len(data) > maxRunPlanBytes {
			return nil, fmt.Errorf("run plan %s exceeds %d bytes", source, maxRunPlanBytes)
		}
		return data, nil
	}
	if path == "-" {
		return read(os.Stdin, "stdin")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read run plan %s: %w", path, err)
	}
	defer file.Close()
	return read(file, path)
}
