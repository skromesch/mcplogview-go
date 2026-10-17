//go:build !unix

package server

import "syscall"

var errAddrInUse error = syscall.Errno(10048) // WSAEADDRINUSE
