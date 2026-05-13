package closer

import "errors"

// Stack is a LIFO list of cleanup callbacks for the "did some work that
// may need to be undone" pattern: callers Add cleanups as they succeed,
// then either drop them on overall success or Run them in reverse order
// on failure. nil cleanups are silently dropped so Add can be called
// unconditionally next to fallible operations.
type Stack []func() error

// Add appends cleanup to the stack when non-nil.
func (s *Stack) Add(cleanup func() error) {
	if cleanup != nil {
		*s = append(*s, cleanup)
	}
}

// Run invokes every cleanup in reverse order and joins any errors.
// Reverse order matches the "undo the last successful step first"
// shape of resource teardown.
func (s Stack) Run() error {
	var errs []error
	for i := len(s) - 1; i >= 0; i-- {
		if err := s[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
