// Command gen writes internal/olderrelease/methods.txt from a tag's
// generated method table: `make older-release TAG=v0.0.19`, run from the
// repository root.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"agent-overflow/internal/olderrelease"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./internal/olderrelease/gen <tag>")
		os.Exit(2)
	}
	tag := os.Args[1]
	src, err := exec.Command("git", "show", tag+":internal/transport/methods_gen.go").Output()
	if err != nil {
		fail("read the method table of %s: %v", tag, err)
	}
	names, err := olderrelease.TableNames(src)
	if err != nil {
		fail("%s: %v", tag, err)
	}
	out := filepath.Join("internal", "olderrelease", "methods.txt")
	if err := os.WriteFile(out, []byte(olderrelease.Format(olderrelease.Release{Tag: tag, Methods: names})), 0o644); err != nil {
		fail("write %s: %v", out, err)
	}
	fmt.Printf("%s: %d methods of %s\n", out, len(names), tag)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "olderrelease: "+format+"\n", args...)
	os.Exit(1)
}
