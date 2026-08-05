// Package appimage removes the AppImage runtime's launch artifacts from
// the environment a child process inherits, so children of an AppImage
// launch resolve against the real system instead of the app's squashfs
// mount.
package appimage
