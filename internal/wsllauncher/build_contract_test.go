package wsllauncher

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The launcher embeds a backend which embeds the SPA. Every public build must
// rebuild in that order, without Task's incomplete globs deciding that Go's
// sources, build flags or embedded data have not changed.
func TestWSLBuildOwnsFrontendAndLeavesGoInputTrackingToGo(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "build", "windows", "Taskfile.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Tasks map[string]struct {
			Method    string      `yaml:"method"`
			Sources   []string    `yaml:"sources"`
			Generates []string    `yaml:"generates"`
			Cmds      []yaml.Node `yaml:"cmds"`
			Deps      []struct {
				Task string `yaml:"task"`
			} `yaml:"deps"`
		} `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	var ordered []string
	var bindingFlags string
	for _, command := range file.Tasks["build:wsl"].Cmds {
		if command.Kind != yaml.MappingNode {
			continue
		}
		var call struct {
			Task string            `yaml:"task"`
			Vars map[string]string `yaml:"vars"`
		}
		if err := command.Decode(&call); err != nil {
			t.Fatal(err)
		}
		if call.Task == "common:build:frontend" {
			bindingFlags = call.Vars["BUILD_FLAGS"]
		}
		if call.Task != "" {
			ordered = append(ordered, call.Task)
		}
	}
	if len(ordered) != 3 || ordered[0] != "common:build:frontend" || ordered[1] != "build:wsl:windows-launcher" || ordered[2] != "build:wsl:restore-payload-placeholder" {
		t.Fatalf("public build must compile frontend before embedding and restore placeholder only after linking: %v", ordered)
	}
	launcher := file.Tasks["build:wsl:windows-launcher"]
	hasPayload := false
	for _, dep := range launcher.Deps {
		hasPayload = hasPayload || dep.Task == "build:wsl:linux-payload"
	}
	if !hasPayload {
		t.Fatal("launcher must build its backend payload before linking")
	}
	for _, name := range []string{"build:wsl:linux-payload", "build:wsl:windows-launcher"} {
		task := file.Tasks[name]
		if task.Method != "none" || len(task.Sources) != 0 || len(task.Generates) != 0 {
			t.Errorf("%s must always invoke Go so changes in imported code, embeds and flags cannot reuse stale bytes", name)
		}
	}
	// A successful generator exit is insufficient: nogui once emitted zero
	// services and erased every App import. Exercise the configured flags and
	// compare the real generated contract with the bindings the frontend uses.
	output := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "tool", "wails3", "generate", "bindings", "-ts", "-f", bindingFlags, "-d", output)
	cmd.Dir = filepath.Join("..", "..")
	if log, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate WSL frontend bindings: %v\n%s", err, log)
	}
	expected := filepath.Join("..", "..", "frontend", "bindings")
	if _, err := os.Stat(filepath.Join(expected, "agent-overflow", "app.ts")); err != nil {
		t.Fatalf("frontend App bindings missing; regenerate before validating: %v", err)
	}
	err = filepath.WalkDir(expected, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".ts" {
			return err
		}
		relative, err := filepath.Rel(expected, path)
		if err != nil {
			return err
		}
		want, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(output, relative))
		if err != nil {
			t.Errorf("WSL generation omitted frontend binding %s: %v", relative, err)
		} else if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))) {
			t.Errorf("WSL generation differs from frontend binding %s", relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
