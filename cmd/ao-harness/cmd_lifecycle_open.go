package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"agent-overflow/internal/externalurl"
)

func runOpen(e *env, args []string) error {
	flags := e.newFlagSet("open")
	browser := flags.Bool("browser", false, "open the URL in the host browser instead of only printing it")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("open takes no positional arguments (got %v)", rest)
	}
	t, err := e.resolveTarget()
	if err != nil {
		return err
	}
	bs, err := bootstrapForTarget(t)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no live instance at %s", t.DataRoot)
		}
		return err
	}
	if e.jsonOutput() {
		if err := e.writeJSON(map[string]any{"id": t.ID, "url": bs.URL}); err != nil {
			return err
		}
	} else {
		e.printf("%s\n", bs.URL)
	}
	if !*browser {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return externalurl.Open(ctx, bs.URL)
}
