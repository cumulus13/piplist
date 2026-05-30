// tty_linux.go — Linux ioctl constant
//go:build linux

package main

import "syscall"

const ioctlGetTermios = syscall.TCGETS
