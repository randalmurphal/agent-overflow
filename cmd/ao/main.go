package main

import (
	"os"

	"agent-overflow/internal/aocli"
)

func main() {
	os.Exit(aocli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
