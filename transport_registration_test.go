package main

import (
	"bufio"
	"context"
	"encoding/json"
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
	if _, err := registerAppMethods(dispatcher, &App{}, transport.RegisterOptions{
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

// TestPreviousReleaseWindowsGetEmptyRetiredAnswers calls retired methods by
// the ids v0.0.19's generated table gives them, as a window on that release
// attached to this computer does.
func TestPreviousReleaseWindowsGetEmptyRetiredAnswers(t *testing.T) {
	dispatcher := transport.NewDispatcher()
	if _, err := registerAppMethods(dispatcher, &App{}, transport.RegisterOptions{
		Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList(),
	}); err != nil {
		t.Fatalf("register App: %v", err)
	}
	for _, released := range []struct {
		name   string
		id     uint32
		scope  string
		params []json.RawMessage
		want   string
	}{
		{"ListDiscussionsForThread", 2502562885, "threads:read", []json.RawMessage{json.RawMessage(`"thread-1"`)}, `[]`},
	} {
		method, frameErr := dispatcher.ResolveForOrigin(released.id, "", false)
		if frameErr != nil {
			t.Errorf("%s: %s", released.name, frameErr.Message)
			continue
		}
		if method.Name != released.name {
			t.Errorf("id %d resolves to %s, want %s", released.id, method.Name, released.name)
		}
		if refused := transport.AuthorizeSessionMethod([]string{released.scope}, method.Name, transport.CallerProof{}); refused != nil {
			t.Errorf("%s with %s: %s", released.name, released.scope, refused.Message)
		}
		answer, frameErr := dispatcher.Invoke(context.Background(), method, released.params)
		if frameErr != nil || string(answer) != released.want {
			t.Errorf("%s answers %s (%v), want %s", released.name, answer, frameErr, released.want)
		}
	}
	// The retired Discussions settings page's list stays unanswered.
	if _, frameErr := dispatcher.ResolveForOrigin(942288562, "ListDiscussions", false); frameErr == nil || frameErr.Code != transport.ErrCodeMethodNotFound {
		t.Errorf("ListDiscussions resolves (%v), want %s", frameErr, transport.ErrCodeMethodNotFound)
	}
}
