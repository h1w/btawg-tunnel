package portmap

import "syscall"

func (r router) validateSnapshot() error                            { return nil }
func (r router) socketControl(_, _ string, _ syscall.RawConn) error { return nil }
