// Command ao-gotest runs the `go test` command line it is given, for `make
// go-test`, inside the test namespace (bin/ao-netns). A package named by a
// -split flag runs as buckets: `go test -list` prints its top-level tests,
// each goes to one of a fixed number of buckets by a hash of its name, and
// each bucket that has tests runs as one `go test -parallel 1 -run
// '^(?:names)$'`. The other packages run in one `go test` beside them.
//
//	ao-gotest [-split importpath=MiB,MiB]... <go command> test [go test flag or package]...
//
// A package is split when its tests open so many SQLite stores that one
// process cannot use more cores: modernc's allocator lock serializes them.
// The bucket count is fixed, not the number of cores, so a bucket's command
// line, and with it its entry in the test cache, is the same whatever ran
// beside it: a result cached at one worker count is cached at any other,
// and a failed bucket reruns without hiding behind another's pass.
//
// Nothing is split, and the command line it was given runs as it is, as one
// `go test`, when no package it names is declared, or when a flag on the
// command line or in GOFLAGS means something else in a bucket than in one
// `go test` of the package. The command line may hold only bucketFlags, and
// GOFLAGS also goflagsBuildFlags; anything else, such as -run, -list, -json,
// -failfast, -p, -cpu, -race, a profile or coverage, runs as one `go test`.
// Packages are matched by import path, as `make go-test` passes them.
//
// A split run reserves memory in the host's memory governor
// (internal/harness/governor) for each of its two phases, which never
// overlap (phase). The list steps run first, one at a time, under a lease
// of the largest list step's declaration; it is released before the
// buckets ask for theirs, which holds W buckets, W being the most the
// governor grants room for (bucketTotals). While a phase is granted
// nothing it says so once and asks again until admitWait has passed, then
// fails: starting a process the host cannot hold is what takes it down.
// The unsplit `go test` runs outside the reservation from the start, as it
// did before this command existed.
//
// Each list step builds its package's test binary once per run, by the `go
// test -o P -list` that reads its names, into a directory of this run's own
// on the go command's work area (runDirParent). Then the buckets run, W at
// a time, packages in declaration order, so the longest starts first. Each
// bucket runs the binary through a hard link of its own (-o), so no bucket
// links. A bucket whose package or a dependency changed after the list
// step builds its own binary over its own link, never over the one the
// other buckets run; the run fails that bucket and says why, since its
// tests ran different code. Each bucket's and list step's largest process
// is read from rusage; one over its package's declaration is named at the
// end of the run, so the declaration can be raised.
//
// Every `go test`, split or not, runs under a leader of its own (lead),
// which ends the processes it started, however its `go test` ended and
// however this process ends; this process waits for every leader before it
// releases the reservation and removes the directory. A process a `go
// test` that ended on its own left running fails the run, named. A signal
// this process receives goes to every leader, which passes it to its tree,
// and no new `go test` starts.
//
// The unsplit `go test` streams its output line by line. A bucket writes
// its output to files in the run's directory, printed whole when it ends
// after a header line, so lines of different processes never interleave
// and no bucket's output is held in memory. The exit status is the worst of
// the `go test` processes'. Each has the -timeout the flags give.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/goenvfile"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == leaderArg {
		os.Exit(lead(os.Args[2:], os.Stdin, os.Stderr))
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// bucketFlags are the go test flags a split run keeps, each with whether it
// takes a value: each means the same in every bucket as in one `go test` of
// the package. -tags and -vet select what builds and is checked, the same
// in every process; the rest filter, repeat, time or report each test.
var bucketFlags = map[string]bool{
	"count":   true,
	"short":   false,
	"shuffle": true,
	"skip":    true,
	"tags":    true,
	"timeout": true,
	"v":       false,
	"vet":     true,
}

// listFlags are the bucketFlags the list step, which builds and vets, takes.
var listFlags = []string{"tags", "vet"}

// goflagsBuildFlags are the build flags GOFLAGS may also set in a split
// run. Every process of the run reads the same GOFLAGS, so they build the
// list step's binary as they build each bucket's, and change nothing else.
// make sets -trimpath there.
var goflagsBuildFlags = []string{"buildvcs", "mod", "modcacherw", "trimpath"}

// split is a -split flag: a package to run as buckets and the bytes one of
// its buckets, and its list step, is declared to hold.
type split struct {
	pkg                  string
	bucketUnit, listUnit uint64
}

type splitFlags []split

func (s *splitFlags) String() string { return fmt.Sprint(*s) }

func (s *splitFlags) Set(value string) error {
	pkg, mibs, ok := strings.Cut(value, "=")
	bucket, list, two := strings.Cut(mibs, ",")
	b, bucketErr := strconv.ParseUint(bucket, 10, 32)
	l, listErr := strconv.ParseUint(list, 10, 32)
	if !ok || !two || pkg == "" || bucketErr != nil || listErr != nil || b == 0 || l == 0 {
		return fmt.Errorf("want importpath=MiB,MiB, what one bucket and the list step each hold in positive whole MiB, got %q", value)
	}
	*s = append(*s, split{pkg: pkg, bucketUnit: b << 20, listUnit: l << 20})
	return nil
}

// parseSplits reads this command's own flags, up to the go command.
func parseSplits(args []string) ([]split, []string, error) {
	var splits splitFlags
	flags := flag.NewFlagSet("ao-gotest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var(&splits, "split", "run the package as buckets, each and its list step declared to hold MiB,MiB")
	if err := flags.Parse(args); err != nil {
		return nil, nil, fmt.Errorf("-split: %w", err)
	}
	return splits, flags.Args(), nil
}

// invocation is a `go test` command line, argv, read as go test reads it.
type invocation struct {
	argv     []string
	flags    []parsedFlag
	packages []string
	// shardable is false when an argument is a flag outside bucketFlags,
	// whose value this command cannot tell from a package.
	shardable bool
}

// parsedFlag is one go test flag: its name without dashes and its value,
// with the arguments it took.
type parsedFlag struct {
	name, value string
	args        []string
}

// parseArgs reads argv, `<go command> test` and its arguments. It reads
// flags as go test does, -name, --name, -name=value or -name value, and
// every other argument as a package.
func parseArgs(argv []string) (invocation, error) {
	if len(argv) < 2 || argv[1] != "test" {
		return invocation{}, fmt.Errorf("usage: ao-gotest [-split importpath=MiB,MiB]... <go command> test [go test flag or package]...; got %q", argv)
	}
	inv := invocation{argv: argv, shardable: true}
	args := argv[2:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			inv.packages = append(inv.packages, arg)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		takesValue, known := bucketFlags[name]
		if !known || (takesValue && !hasValue && i+1 == len(args)) {
			inv.shardable = false
			return inv, nil
		}
		taken := []string{arg}
		if takesValue && !hasValue {
			i++
			value = args[i]
			taken = append(taken, value)
		}
		inv.flags = append(inv.flags, parsedFlag{name: name, value: value, args: taken})
	}
	return inv, nil
}

// goflagsSplit reports whether value, GOFLAGS, leaves a run splittable.
// The go command reads it as fields of -flag or -flag=value, a field
// quoted when it holds a space, and applies each to every go command that
// knows the flag, the list step and every bucket alike; test flags may
// carry a "test." prefix. A field this command cannot read is the go
// command's to report, so the run is not split.
func goflagsSplit(value string) bool {
	fields, ok := splitQuoted(value)
	if !ok {
		return false
	}
	for _, field := range fields {
		name, _, _ := strings.Cut(field, "=")
		if !strings.HasPrefix(name, "-") {
			return false
		}
		name = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(name, "-"), "-"), "test.")
		if _, known := bucketFlags[name]; !known && !slices.Contains(goflagsBuildFlags, name) {
			return false
		}
	}
	return true
}

// splitQuoted splits s as the go command splits GOFLAGS (cmd/internal/quoted):
// at spaces, tabs, carriage returns and newlines, a field that starts with
// a quote running to the next of that quote, quotes left out. ok is false
// for a quote left open.
func splitQuoted(s string) (fields []string, ok bool) {
	const space = " \t\r\n"
	for {
		s = strings.TrimLeft(s, space)
		if s == "" {
			return fields, true
		}
		if quote := s[0]; quote == '"' || quote == '\'' {
			end := strings.IndexByte(s[1:], quote)
			if end < 0 {
				return nil, false
			}
			fields = append(fields, s[1:1+end])
			s = s[2+end:]
			continue
		}
		end := strings.IndexAny(s, space)
		if end < 0 {
			end = len(s)
		}
		fields = append(fields, s[:end])
		s = s[end:]
	}
}

func run(argv []string, stdout, stderr io.Writer) int {
	splits, rest, err := parseSplits(argv)
	if err == nil {
		var inv invocation
		inv, err = parseArgs(rest)
		if err == nil {
			return runInvocation(inv, splits, stdout, stderr)
		}
	}
	fmt.Fprintln(stderr, "ao-gotest:", err)
	return 2
}

func runInvocation(inv invocation, splits []split, stdout, stderr io.Writer) int {
	goflags, err := goenvfile.Getenv("GOFLAGS")
	if err != nil {
		fmt.Fprintln(stderr, "ao-gotest: read GOFLAGS as the go command does:", err)
		return 1
	}
	// Declaration order is the order the packages are listed and their
	// buckets start in, so the longest goes first.
	var declared []split
	var names []string
	for _, s := range splits {
		if slices.Contains(inv.packages, s.pkg) {
			declared = append(declared, s)
			names = append(names, s.pkg)
		}
	}
	r, err := newRunner(inv.argv[0], stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "ao-gotest:", err)
		return 1
	}
	defer r.close()
	stopSignals := r.forwardSignals()
	if inv.shardable && goflagsSplit(goflags) && len(declared) > 0 {
		whole := slices.DeleteFunc(slices.Clone(inv.packages), func(pkg string) bool { return slices.Contains(names, pkg) })
		r.runSplit(inv, declared, whole)
	} else {
		r.runWhole(job{args: inv.argv[1:]})
	}
	stopSignals()
	r.printNotes()
	return r.status()
}
