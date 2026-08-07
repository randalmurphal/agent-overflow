// Package selfupdate carries the cross-process contract the WSL backend and the
// Windows launcher share to hand a downloaded release across the boundary: the
// install directive, the staging-directory copy/sweep helpers, the
// "swap never applied" marker, and the local-file updater.Provider the launcher
// drives to reuse the stock swap machinery.
package selfupdate
