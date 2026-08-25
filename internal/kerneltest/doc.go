// Package kerneltest holds the importable half of the provider-spawn
// isolation every fixture that can construct a session-capable App must
// install: a poisoned provider binary plus its spawn tripwire, a detached
// HOME/USERPROFILE, and the stubs for the two side-effect spawn paths
// (Codex model catalog, text generation).
package kerneltest
