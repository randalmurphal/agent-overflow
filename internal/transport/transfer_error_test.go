package transport

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type transferTestError struct{ moved bool }

func (e *transferTestError) Error() string { return "private source path /home/secret" }
func (e *transferTestError) ThreadTransferRef() (string, string, bool) {
	return "operation", "destination", e.moved
}

type transferErrorApp struct{ moved bool }

func (a *transferErrorApp) Send() error {
	return fmt.Errorf("internal detail: %w", &transferTestError{moved: a.moved})
}

func TestTransferRefusalReachesRemoteClientWithoutInternalDetails(t *testing.T) {
	for _, moved := range []bool{false, true} {
		d := NewDispatcher()
		methods, err := d.Register(&transferErrorApp{moved: moved}, RegisterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, frame := d.InvokeForOrigin(context.Background(), methods[0], nil, false)
		code := ErrCodeThreadTransferPending
		if moved {
			code = ErrCodeThreadMoved
		}
		if frame == nil || frame.Code != code || frame.Transfer == nil || frame.Transfer.OperationID != "operation" || frame.Transfer.BackendID != "destination" {
			t.Fatalf("refusal: %+v", frame)
		}
		if strings.Contains(frame.Message, "secret") || strings.Contains(frame.Message, "internal") {
			t.Fatalf("leaked private diagnostic: %+v", frame)
		}
	}
}
