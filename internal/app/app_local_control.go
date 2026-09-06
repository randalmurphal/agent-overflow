package app

import (
	"agent-overflow/internal/localcontrol"
	"log"
)

func (a *App) publishLocalControl() {
	srv := a.transportServer.Load()
	if srv == nil || a.configDir == "" {
		return
	}
	if err := localcontrol.Publish(a.configDir, srv.Addr(), srv.Token()); err != nil {
		log.Printf("local pairing console unavailable: %v", err)
	}
}
func (a *App) withdrawLocalControl() {
	srv := a.transportServer.Load()
	if srv == nil || a.configDir == "" {
		return
	}
	if err := localcontrol.Withdraw(a.configDir, srv.Token()); err != nil {
		log.Printf("local pairing console cleanup: %v", err)
	}
}
