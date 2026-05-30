// tty_unix.go — TTY detection for Linux and macOS
//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || solaris

package main

import (
	"syscall"
	"unsafe"
)

func isTerminalFd(fd uintptr) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd,
		ioctlGetTermios, uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}
