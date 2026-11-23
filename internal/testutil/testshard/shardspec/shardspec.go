// Package shardspec is the contract between testshard.Select and
// cmd/ao-gotest, which starts the processes Select restricts: the variable
// naming a process's shard and its format. It is apart from testshard so
// that importing testshard means calling Select: cmd/ao-gotest splits a
// package whose test files import SelectPackage, and a package split that
// does not call Select would run every test in each of its processes.
package shardspec

import (
	"fmt"
	"strconv"
	"strings"
)

// Env names the shard a process runs: "i/K", with 0 <= i < K.
const Env = "AO_TEST_SHARD"

// SelectPackage is the import path of the package whose Select a package
// calls from TestMain to be split.
const SelectPackage = "agent-overflow/internal/testutil/testshard"

// Format is the Env value of shard i of count.
func Format(shard, count int) string { return fmt.Sprintf("%d/%d", shard, count) }

// Parse reads an Env value: two decimal numbers "i/K" with K >= 1 and
// 0 <= i < K.
func Parse(spec string) (shard, count int, err error) {
	left, right, found := strings.Cut(spec, "/")
	shard, errShard := decimal(left)
	count, errCount := decimal(right)
	if !found || errShard != nil || errCount != nil || count < 1 || shard >= count {
		return 0, 0, fmt.Errorf("%s=%q: want i/K with K >= 1 and 0 <= i < K", Env, spec)
	}
	return shard, count, nil
}

func decimal(s string) (int, error) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not a decimal number", s)
	}
	return strconv.Atoi(s)
}
