package app

import "os"

func openRemoteArtifact(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
