package threadapp

import (
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

type ModeUpdate struct {
	Thread store.Thread
	Mode   string
}

func (s *Service) UpdateMode(threadID, mode string) (ModeUpdate, error) {
	database, err := s.database("update thread mode")
	if err != nil {
		return ModeUpdate{}, err
	}
	normalized, err := threadmode.ValidateSet(mode)
	if err != nil {
		return ModeUpdate{}, err
	}
	current, err := database.GetThread(threadID)
	if err != nil {
		return ModeUpdate{}, err
	}
	if !threadmode.IsPostCreationMode(current.Mode) {
		return ModeUpdate{}, fmt.Errorf("cannot change mode of %q thread (immutable thread type)", current.Mode)
	}
	if err := database.UpdateMode(threadID, normalized); err != nil {
		return ModeUpdate{}, err
	}
	thread, err := database.GetThread(threadID)
	if err != nil {
		return ModeUpdate{}, err
	}
	return ModeUpdate{Thread: thread, Mode: normalized}, nil
}
