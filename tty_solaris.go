// tty_solaris.go — Solaris ioctl constant
//go:build solaris

package main

import "syscall"

const ioctlGetTermios = syscall.TCGETS
