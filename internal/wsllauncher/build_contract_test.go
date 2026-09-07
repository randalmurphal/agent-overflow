package wsllauncher

import (
	"os"
	"path/filepath"
	"testing"

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
		if call.Task == "common:build:frontend" && call.Vars["BUILD_FLAGS"] != "-tags nogui" {
			t.Fatal("WSL binding generation must not require Linux desktop libraries")
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
}
