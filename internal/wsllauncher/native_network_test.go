package wsllauncher

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"agent-overflow/internal/nativenetwork"
	"agent-overflow/internal/webview2host"
	"github.com/coder/websocket"
)

func TestNativeNetworkConfigurationAndReportUseExistingOwnerRPC(t *testing.T) {
	config := nativenetwork.Config{Enabled: true, PairingOpen: true, BackendID: "backend", Name: "Windows", Target: "https://172.20.1.2:4141", ScanID: 2}
	state := nativenetwork.State{Addresses: []string{"https://192.168.1.9:4141"}, ScanID: 2}
	reported := make(chan nativenetwork.State, 1)
	wsURL := startBridgeStub(t, func(ctx context.Context, conn *websocket.Conn, _ int) error {
		if err := expectBrowserHostSubscribeAndReplay(ctx, conn); err != nil {
			return err
		}
		for call := 0; call < 3; call++ {
			frame, err := readClientFrame(ctx, conn)
			if err != nil {
				return err
			}
			reply := notificationServerFrame{Type: "rpc", ID: frame.ID}
			switch call {
			case 0:
				if frame.Method != "GetNativeNetworkConfig" || len(frame.Params) != 0 {
					return fmt.Errorf("unexpected config RPC: %+v", frame)
				}
				reply.Result, _ = json.Marshal(config)
			case 1:
				if frame.Method != "ReportNativeNetworkState" || len(frame.Params) != 1 {
					return fmt.Errorf("unexpected report RPC: %+v", frame)
				}
				var got nativenetwork.State
				if err := json.Unmarshal(frame.Params[0], &got); err != nil {
					return err
				}
				reported <- got
			case 2:
				reply.Result = json.RawMessage(`"bad response"`)
			}
			if err := writeServerFrame(ctx, conn, reply); err != nil {
				return err
			}
		}
		conn.Read(ctx)
		return nil
	})
	client, _ := newTestBrowserHostClient(t, wsURL, func(webview2host.Directive) {})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go client.Run(ctx)
	got, err := client.GetNativeNetworkConfig(ctx)
	if err != nil || got != config {
		t.Fatalf("config=%+v error=%v", got, err)
	}
	if err := client.ReportNativeNetworkState(ctx, state); err != nil {
		t.Fatal(err)
	}
	if got := <-reported; !reflect.DeepEqual(got, state) {
		t.Fatalf("reported %+v", got)
	}
	if _, err := client.GetNativeNetworkConfig(ctx); err == nil {
		t.Fatal("accepted malformed config reply")
	}
}
