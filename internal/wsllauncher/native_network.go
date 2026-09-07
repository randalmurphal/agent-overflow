package wsllauncher

import (
	"context"
	"encoding/json"

	"agent-overflow/internal/nativenetwork"
)

func (c *NotificationClient) GetNativeNetworkConfig(ctx context.Context) (nativenetwork.Config, error) {
	ctx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
	defer cancel()
	var config nativenetwork.Config
	err := c.callRPC(ctx, "native-network", "GetNativeNetworkConfig", nil, &config)
	return config, err
}
func (c *NotificationClient) ReportNativeNetworkState(ctx context.Context, state nativenetwork.State) error {
	ctx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
	defer cancel()
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return c.callRPC(ctx, "native-network", "ReportNativeNetworkState", []json.RawMessage{encoded})
}
