// ao-mockforge stands in for gh and glab in an isolated boot (--harness,
// --soak). internal/git runs it in place of either CLI and tells it which
// one through AO_FORGE_CLI. It holds no forge behavior: it forwards its
// argv, working directory and stdin over the harness control channel and
// prints the harness's answer (internal/harness/forgefake), exiting with
// the answer's status.
//
// Outside a harness, with no control channel in its environment, it
// refuses every invocation. It never falls back to a real CLI.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/control"
)

func main() {
	os.Exit(run(os.Args, os.Stdin, os.Stdout, os.Stderr))
}

func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cli := os.Getenv(gitops.ForgeCLINameEnv)
	if cli == "" && len(argv) > 0 {
		cli = filepath.Base(argv[0])
	}
	args := argv[1:]
	if cli != "gh" && cli != "glab" {
		fmt.Fprintf(stderr, "ao-mockforge: %s is unset and argv[0] is %q; run me as gh or glab\n", gitops.ForgeCLINameEnv, cli)
		return 2
	}
	client, ok := control.FromEnv()
	if !ok {
		fmt.Fprintf(stderr, "ao-mockforge: %s: no harness control channel (%s, %s); this fake answers only inside an isolated boot\n",
			formatArgv(cli, args), control.EnvAddr, control.EnvToken)
		return 1
	}
	input, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "ao-mockforge: %s: read stdin: %v\n", formatArgv(cli, args), err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "ao-mockforge: %s: getwd: %v\n", formatArgv(cli, args), err)
		return 1
	}
	result, err := client.Forge(control.ForgeCall{CLI: cli, Args: args, Cwd: cwd, Stdin: input, PID: os.Getpid()})
	if err != nil {
		fmt.Fprintf(stderr, "ao-mockforge: %s: the harness did not answer: %v\n", formatArgv(cli, args), err)
		return 1
	}
	if _, err := stdout.Write(result.Stdout); err != nil {
		fmt.Fprintf(stderr, "ao-mockforge: write stdout: %v\n", err)
		return 1
	}
	if _, err := io.WriteString(stderr, result.Stderr); err != nil {
		return 1
	}
	return result.ExitCode
}

func formatArgv(cli string, args []string) string {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, cli)
	for _, arg := range args {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return strings.Join(quoted, " ")
}
