package sshsetup

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"unicode"

	"agent-overflow/internal/procutil"
)

type Request struct {
	Target       string `json:"target"`
	Binary       string `json:"binary"`
	StartService bool   `json:"startService"`
	LAN          bool   `json:"lan"`
}
type Runner interface {
	Run(context.Context, Request, string, io.Reader, io.Writer, io.Writer) error
}
type OSRunner struct{}

func validate(request Request) error {
	if request.Target == "" || len(request.Target) > 255 || strings.HasPrefix(request.Target, "-") {
		return errors.New("enter an SSH host or configured alias")
	}
	for _, ch := range request.Target {
		if !unicode.IsLetter(ch) && !unicode.IsDigit(ch) && !strings.ContainsRune("@._-:[]", ch) {
			return errors.New("SSH host must contain only a host, user@host, or configured alias")
		}
	}
	if request.Binary == "" || len(request.Binary) > 4096 || strings.ContainsAny(request.Binary, "\x00\r\n") {
		return errors.New("enter the remote Agent Overflow executable path")
	}
	return nil
}
func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func Arguments(request Request, action string) ([]string, error) {
	if err := validate(request); err != nil {
		return nil, err
	}
	command := quote(request.Binary)
	switch action {
	case "pair":
		command += " pair --json --class desktop --wait 30s"
		if request.LAN {
			command += " --lan"
		}
	case "start":
		command += " service start"
	default:
		return nil, errors.New("unknown SSH setup action")
	}
	return []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2", "-o", "PermitLocalCommand=no",
		"-o", "ClearAllForwardings=yes", "-o", "RemoteCommand=none", "--", request.Target, command}, nil
}
func (OSRunner) Run(ctx context.Context, request Request, action string, stdin io.Reader, stdout, stderr io.Writer) error {
	if testing.Testing() {
		return errors.New("tests must inject an SSH runner")
	}
	args, err := Arguments(request, action)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "ssh", args...)
	procutil.ConfigureGroup(command)
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	return command.Run()
}
