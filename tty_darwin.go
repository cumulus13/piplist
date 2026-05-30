// tty_darwin.go — macOS/BSD ioctl constant
//go:build darwin || freebsd || openbsd || netbsd || dragonfly

package main

import "syscall"

const ioctlGetTermios = syscall.TIOCGETA
