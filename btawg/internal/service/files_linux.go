package service

import "os"

func Privileged() bool { return os.Geteuid() == 0 }

func WriteManagedFile(path string, data []byte) error { return WriteFile(path, data, 0o600) }
