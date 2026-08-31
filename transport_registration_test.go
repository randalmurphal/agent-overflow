package main

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"testing"

	"agent-overflow/internal/harnessrpc"
	"agent-overflow/internal/transport"
)

func TestGeneratedWailsBindingsKeepTransportMethodIDs(t *testing.T) {
	file, err := os.Open("frontend/bindings/agent-overflow/app.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	functionPattern := regexp.MustCompile(`^export function ([A-Za-z0-9_]+)\(`)
	idPattern := regexp.MustCompile(`\$Call\.ByID\(([0-9]+)`)
	generated := make(map[string]uint32, len(transport.GeneratedMethods))
	currentMethod := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if match := functionPattern.FindStringSubmatch(line); match != nil {
			if currentMethod != "" {
				t.Fatalf("generated binding %s has no $Call.ByID before %s", currentMethod, match[1])
			}
			currentMethod = match[1]
			continue
		}
		if currentMethod == "" {
			continue
		}
		match := idPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		parsed, err := strconv.ParseUint(match[1], 10, 32)
		if err != nil {
			t.Fatalf("parse generated binding id for %s: %v", currentMethod, err)
		}
		generated[currentMethod] = uint32(parsed)
		currentMethod = ""
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if currentMethod != "" {
		t.Fatalf("generated binding %s has no $Call.ByID", currentMethod)
	}
	if len(generated) != len(transport.GeneratedMethods) {
		t.Fatalf("generated Wails methods = %d, transport methods = %d", len(generated), len(transport.GeneratedMethods))
	}
	for _, method := range transport.GeneratedMethods {
		if got, ok := generated[method.Name]; !ok || got != method.ID {
			t.Errorf("generated Wails id for %s = %d (present=%v), want %d", method.Name, got, ok, method.ID)
		}
	}
}

// TestProductionReceiversRegisterWithoutCollision registers the two real
// receivers (App, Harness) on one dispatcher exactly as bootTransport does.
// Name-based dispatch shares one namespace across receivers, so a future
// App method that shadows a Harness method (or vice versa) must fail here
// at `make go-test` time rather than at `--harness` boot.
func TestProductionReceiversRegisterWithoutCollision(t *testing.T) {
	dispatcher := transport.NewDispatcher()
	if _, err := dispatcher.Register(&App{}, transport.RegisterOptions{
		Package:   "main",
		TypeName:  "App",
		AllowList: transport.NewMethodAllowList(),
	}); err != nil {
		t.Fatalf("register App: %v", err)
	}
	_, err := dispatcher.Register(harnessrpc.New(harnessrpc.Config{}), transport.RegisterOptions{
		Package:   "main",
		TypeName:  "Harness",
		LocalOnly: true,
	})
	if err != nil {
		t.Fatalf("register Harness: %v", err)
	}
}
