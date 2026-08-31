package harnessrpc

import (
	"context"
	"log"
)

// HarnessShutdown is the authenticated lifecycle door used by ao-harness
// down. The caller has already proved it reached this backend with the token
// from this root's instance file. Shutdown runs asynchronously so the RPC
// response can be delivered before the transport closes.
func (h *Harness) HarnessShutdown() error {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), h.config.ShutdownTimeout)
		defer cancel()
		if h.config.Host != nil {
			if err := h.config.Host.Shutdown(ctx); err != nil {
				log.Printf("harness: authenticated shutdown: %v", err)
			}
		}
		h.mu.Lock()
		removeInstance := h.removeInstance
		h.mu.Unlock()
		if removeInstance != nil {
			removeInstance()
		}
		if h.config.TerminateSelf != nil {
			if err := h.config.TerminateSelf(); err != nil {
				log.Printf("harness: authenticated shutdown process exit: %v", err)
			}
		}
	}()
	return nil
}
