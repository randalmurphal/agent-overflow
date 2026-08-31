package threadapp

import "os"

func symlinkForTest(oldname, newname string) error { return os.Symlink(oldname, newname) }
