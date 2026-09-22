package triage

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestCodexRuntimeActivityPreservesProviderMessage(t *testing.T) {
	for _, activity := range []string{"poll", "command", "completion-only", "stdin"} {
		for _, stream := range []string{"text", "thinking"} {
			for _, scope := range []string{"", "agent-1"} {
				t.Run(activity+"/"+stream+"/"+scope, func(t *testing.T) {
					router, st, _ := newTestRouter(t)
					createTestThread(t, st, "t1")
					if err := st.UpdateProvider("t1", "codex"); err != nil {
						t.Fatal(err)
					}
					seedOpenTurn(t, router, st, "t1", 0)
					deltaKind, itemKind := provider.EventTextDelta, itemKindAssistantText
					if stream == "thinking" {
						deltaKind, itemKind = provider.EventThinking, itemKindThinking
					}
					seedTerminalInteractionBackgroundExec(t, router, "t1", "pid-1", "cmd-1", "sleep 10")
					handle := func(evt provider.ProviderEvent) {
						t.Helper()
						evt.ThreadID = "t1"
						evt.Timestamp = time.Now()
						if err := router.Handle(evt); err != nil {
							t.Fatal(err)
						}
					}
					handle(provider.ProviderEvent{Kind: deltaKind, ItemID: "answer", ParentToolUseID: scope, Content: "One complete "})
					switch activity {
					case "poll", "stdin":
						stdin := ""
						if activity == "stdin" {
							stdin = "secret input"
						}
						handle(provider.ProviderEvent{Kind: provider.EventTerminalInteraction, ItemID: "cmd-1", Meta: terminalInteractionMetaBlob(t, "pid-1", stdin)})
					case "command":
						handle(provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "cmd-2", ItemType: "commandExecution", Meta: buildUnifiedExecStartMeta(t, "pid-2", "echo later")})
					case "completion-only":
						handle(provider.ProviderEvent{Kind: provider.EventToolComplete, ItemID: "close-1", ItemType: "close_agent", Meta: []byte(`{"toolName":"close_agent"}`)})
					}
					before := findItemsByKind(t, st, "t1", itemKind)
					if len(before) != 1 || before[0].Status != statusStreaming {
						t.Fatalf("runtime activity ended provider message: %+v", before)
					}
					handle(provider.ProviderEvent{Kind: deltaKind, ItemID: "answer", ParentToolUseID: scope, Content: "answer."})
					stop := provider.ProviderEvent{Kind: provider.EventContentBlockStop, ItemID: "answer", ParentToolUseID: scope, Content: "One complete answer.", ContentPresent: true, Meta: []byte(`{"blockType":"` + stream + `"}`)}
					handle(stop)
					router.settleWG.Wait()
					handle(stop)
					router.settleWG.Wait()
					items := findItemsByKind(t, st, "t1", itemKind)
					if len(items) != 1 || items[0].Summary != "One complete answer." || items[0].Status != statusCompleted {
						t.Fatalf("provider message fragmented or duplicated: %+v", items)
					}
					if activity == "poll" && len(findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))) != 0 {
						t.Fatal("empty poll created history")
					}
				})
			}
		}
	}
}
