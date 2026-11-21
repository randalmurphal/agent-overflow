package adf

import (
	"regexp"
	"sync"
	"sync/atomic"
)

// The package's tables and expressions are built at their first use, once,
// so loading the package builds none of them: trackerapp's TestInitCost
// holds its init to registering one goldmark node kind. A
// package variable of either type below is declared with constant fields,
// which the compiler lays out without init code. After the first use,
// get is one atomic load.

// lazyRegexp is a regular expression compiled at its first use. A pattern
// that does not compile panics there; TestLazyRegexpsCompile reaches every
// one.
type lazyRegexp struct {
	pattern string
	once    sync.Once
	re      atomic.Pointer[regexp.Regexp]
}

func (l *lazyRegexp) get() *regexp.Regexp {
	if re := l.re.Load(); re != nil {
		return re
	}
	l.once.Do(func() { l.re.Store(regexp.MustCompile(l.pattern)) })
	return l.re.Load()
}

// lazy is a value built by build at its first use. The value is shared:
// no caller changes it.
type lazy[T any] struct {
	build func() T
	once  sync.Once
	v     atomic.Pointer[T]
}

func (l *lazy[T]) get() T {
	if v := l.v.Load(); v != nil {
		return *v
	}
	l.once.Do(func() {
		v := l.build()
		l.v.Store(&v)
	})
	return *l.v.Load()
}
