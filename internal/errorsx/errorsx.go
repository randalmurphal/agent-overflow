package errorsx

import "fmt"

// Append returns errs unchanged when err is nil and appends err
// otherwise. The nil filter keeps `errors.Join(errs...)` clean at the
// end of lifecycle teardown paths that gather independent failures.
func Append(errs []error, err error) []error {
	if err == nil {
		return errs
	}
	return append(errs, err)
}

// WrapLifecycle returns nil when cause is nil; otherwise prepends action
// as a `%s: %w` context. Used by App startup/shutdown sequences so a
// failure from a closer reads as "close store after logger init
// failure: <inner cause>" rather than just the raw underlying error.
func WrapLifecycle(action string, cause error) error {
	if cause == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, cause)
}
