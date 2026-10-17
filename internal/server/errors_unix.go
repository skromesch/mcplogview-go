//go:build unix

package server

import "syscall"

var errAddrInUse error = syscall.EADDRINUSE
