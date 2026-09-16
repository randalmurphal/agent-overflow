package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// parkedApp is a receiver whose only RPC blocks until the test releases
// it — the store stall the per-connection cap exists to survive,
// reproduced without a store.
type parkedApp struct {
	entered chan struct{}

	releaseOnce sync.Once
	release     chan struct{}
}

func newParkedApp() *parkedApp {
	return &parkedApp{entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (a *parkedApp) Park() error {
	a.entered <- struct{}{}
	<-a.release
	return nil
}

func (a *parkedApp) unpark() { a.releaseOnce.Do(func() { close(a.release) }) }

// TestConnRefusesRPCsPastTheCapAndKeepsReading is the read-loop liveness
// pin. With every RPC slot held by a parked handler, the connection must
// still answer the next RPC — promptly, with client_overloaded — and must
// still apply the ordinary non-RPC frames a client keeps sending.
// Otherwise one stalled call takes the whole socket down with it and the
// client sees neither an error nor a disconnect.
func TestConnRefusesRPCsPastTheCapAndKeepsReading(t *testing.T) {
	const slots = 2
	app := newParkedApp()
	f := newServerFixtureWith(t, func(cfg *Config) {
		d := NewDispatcher()
		if _, err := d.Register(app, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
			t.Fatalf("register parked receiver: %v", err)
		}
		cfg.Dispatcher = d
		cfg.MaxConcurrentRPCs = slots
		// Long enough that no spontaneous heartbeat can be mistaken for
		// the pong this test uses as its read-loop probe.
		cfg.KeepaliveInterval = time.Minute
	})
	// Registered after the fixture so it runs BEFORE the fixture's
	// shutdown: connection teardown waits for in-flight handlers, and a
	// failed assertion must not leave that wait parked forever.
	t.Cleanup(app.unpark)

	conn := f.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	write := func(frame ClientFrame) {
		t.Helper()
		buf, err := json.Marshal(frame)
		if err != nil {
			t.Fatalf("marshal %s frame: %v", frame.Type, err)
		}
		if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
			t.Fatalf("write %s frame: %v", frame.Type, err)
		}
	}
	// read returns the next frame that carries a verdict. The hello frame
	// every connection opens with is the one thing skipped; anything else
	// arriving out of turn is a finding, not noise.
	read := func() ServerFrame {
		t.Helper()
		for {
			readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
			_, raw, err := conn.Read(readCtx)
			readCancel()
			if err != nil {
				t.Fatalf("ws read: %v", err)
			}
			var frame ServerFrame
			if err := json.Unmarshal(raw, &frame); err != nil {
				t.Fatalf("decode server frame: %v", err)
			}
			if frame.Type == frameTypeHello {
				continue
			}
			return frame
		}
	}

	// Fill every slot and wait until the handlers are actually inside the
	// method, so the next frame is refused for the cap rather than racing it.
	parked := make([]string, 0, slots)
	for i := range slots {
		id := fmt.Sprintf("park-%d", i)
		parked = append(parked, id)
		write(ClientFrame{Type: frameTypeRPC, ID: id, Method: "Park"})
	}
	for range slots {
		select {
		case <-app.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("parked handlers never entered the method")
		}
	}

	// One more RPC. It must come back refused, not silently queued behind
	// the parked handlers.
	write(ClientFrame{Type: frameTypeRPC, ID: "over-cap", Method: "Park"})
	refused := read()
	if refused.Type != frameTypeRPC || refused.ID != "over-cap" {
		t.Fatalf("first reply = %+v, want the over-cap RPC's answer", refused)
	}
	if refused.Error == nil || refused.Error.Code != ErrCodeClientOverloaded {
		t.Fatalf("over-cap reply error = %+v, want code %q", refused.Error, ErrCodeClientOverloaded)
	}
	if refused.Error.Message == "" {
		t.Fatal("over-cap refusal carries no message for the caller to render")
	}
	select {
	case <-app.entered:
		t.Fatal("refused RPC still reached the method")
	default:
	}

	// Unrelated frames must still be processed while the handlers park.
	// The subscribe narrows this connection to one channel; the ping behind
	// it proves the read loop consumed the subscribe, because the loop
	// answers frames in the order they arrive.
	write(ClientFrame{Type: frameTypeSubscribe, Channels: []string{"ch:watched"}})
	write(ClientFrame{Type: frameTypePing})
	if pong := read(); pong.Type != frameTypePing {
		t.Fatalf("frame after subscribe = %+v, want a heartbeat", pong)
	}

	if _, err := f.bus.Emit("ch:unwatched", "filtered out"); err != nil {
		t.Fatalf("emit unwatched: %v", err)
	}
	if _, err := f.bus.Emit("ch:watched", "delivered"); err != nil {
		t.Fatalf("emit watched: %v", err)
	}
	event := read()
	if event.Type != frameTypeEvent || event.Channel != "ch:watched" {
		t.Fatalf("event frame = %+v, want the watched channel (the subscribe was not applied)", event)
	}

	// The parked handlers still own their slots and still answer once
	// released: refusing the overflow must not have dropped them.
	app.unpark()
	answered := map[string]bool{}
	for range parked {
		frame := read()
		if frame.Type != frameTypeRPC {
			t.Fatalf("frame while draining parked RPCs = %+v", frame)
		}
		if frame.Error != nil {
			t.Fatalf("parked RPC %s failed: %+v", frame.ID, frame.Error)
		}
		answered[frame.ID] = true
	}
	for _, id := range parked {
		if !answered[id] {
			t.Fatalf("parked RPC %s never answered", id)
		}
	}
}
