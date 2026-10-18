//go:build unix

package plugin

import "syscall"

// openExtraFlags: never block on a swapped-in FIFO, never follow a final
// symlink (the path is already resolved).
const openExtraFlags = syscall.O_NONBLOCK | syscall.O_NOFOLLOW
