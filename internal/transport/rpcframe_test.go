package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

type rpcFrameParityReceiver struct{}

func (*rpcFrameParityReceiver) Void() {}
func (*rpcFrameParityReceiver) Echo(s string) (map[string]any, error) {
	return map[string]any{"msg": s, "html": "<tag>&\"quote\"", "nums": []int{1, 2, 3}}, nil
}
func (*rpcFrameParityReceiver) Multi(a int, b string) (int, string) {
	return a, b
}

type panickingJSONValue struct{}

func (panickingJSONValue) MarshalJSON() ([]byte, error) {
	panic("boom during marshal")
}

func (*rpcFrameParityReceiver) RawNil() json.RawMessage {
	return nil
}
func (*rpcFrameParityReceiver) PanicMarshal() panickingJSONValue {
	return panickingJSONValue{}
}

func TestInvokeToRPCFrameMatchesServerFrameMarshalByteForByte(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&rpcFrameParityReceiver{}, RegisterOptions{Package: "test", TypeName: "R"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	cases := []struct {
		name   string
		method string
		id     string
		params []json.RawMessage
	}{
		{name: "void-ascii-id", method: "Void", id: "rpc-1"},
		{name: "void-empty-id", method: "Void", id: ""},
		{name: "echo-special-id", method: "Echo", id: "id<1>&\"2\"\n", params: []json.RawMessage{json.RawMessage(`"hello"`)}},
		{name: "echo-unicode-id", method: "Echo", id: "id-é-🚀", params: []json.RawMessage{json.RawMessage(`"world"`)}},
		{name: "multi-return", method: "Multi", id: "rpc-9", params: []json.RawMessage{json.RawMessage(`42`), json.RawMessage(`"ok"`)}},
		{name: "raw-nil", method: "RawNil", id: "rpc-10"},
	}

	var buf bytes.Buffer
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := d.LookupName(tc.method)
			if !ok {
				t.Fatalf("missing method %s", tc.method)
			}
			res, fe := d.Invoke(context.Background(), m, tc.params)
			if fe != nil {
				t.Fatalf("Invoke failed: %+v", fe)
			}
			want, err := json.Marshal(ServerFrame{
				Type:   frameTypeRPC,
				ID:     tc.id,
				Result: res,
			})
			if err != nil {
				t.Fatalf("Marshal want: %v", err)
			}

			buf.Reset()
			if fe := d.invokeToRPCFrame(context.Background(), m, tc.id, tc.params, &buf); fe != nil {
				t.Fatalf("invokeToRPCFrame failed: %+v", fe)
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Fatalf("wire mismatch:\n got: %s\nwant: %s", buf.Bytes(), want)
			}
		})
	}
}

func TestDispatcherPanicDuringResultMarshalRecovered(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&rpcFrameParityReceiver{}, RegisterOptions{Package: "test", TypeName: "R"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	m, ok := d.LookupName("PanicMarshal")
	if !ok {
		t.Fatal("missing PanicMarshal")
	}

	res, fe := d.Invoke(context.Background(), m, nil)
	if fe == nil || fe.Code != ErrCodeInternal {
		t.Fatalf("Invoke panic during marshal: got (%q, %+v), want ErrCodeInternal", res, fe)
	}

	var buf bytes.Buffer
	fe = d.invokeToRPCFrame(context.Background(), m, "rpc-panic", nil, &buf)
	if fe == nil || fe.Code != ErrCodeInternal {
		t.Fatalf("invokeToRPCFrame panic during marshal: got %+v, want ErrCodeInternal", fe)
	}
}
