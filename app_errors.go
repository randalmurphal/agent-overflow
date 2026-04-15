package main

import "fmt"

func appendError(errs []error, err error) []error {
	if err == nil {
		return errs
	}
	return append(errs, err)
}

func wrapLifecycleError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}
