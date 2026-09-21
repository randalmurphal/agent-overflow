package app

import "fmt"

// A fork interrupted before publication cannot be resumed as a conversation.
// Boot removes its private cache before admitting clients or provider work.
func (a *App) cleanupPreparingForks() error {
	ids, err := a.store.ListPreparingForks()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := a.cleanupForkThread(id); err != nil {
			return fmt.Errorf("clean interrupted fork %s: %w", id, err)
		}
	}
	return nil
}
