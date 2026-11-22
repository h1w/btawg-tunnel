package service

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockUpdate(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func prepareUpdateLock(string) error { return nil }
