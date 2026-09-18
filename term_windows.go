//go:build windows
// +build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

const (
	enableEchoInput      = 0x0004
	enableLineInput      = 0x0002
	enableProcessedInput = 0x0001
)

func makeRaw(fd int) (interface{}, error) {
	var oldMode uint32
	handle := syscall.Handle(fd)
	r, _, err := procGetConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&oldMode)))
	if r == 0 {
		return nil, fmt.Errorf("getconsolemode: %v", err)
	}
	newMode := oldMode &^ (enableEchoInput | enableLineInput | enableProcessedInput)
	r, _, err = procSetConsoleMode.Call(uintptr(handle), uintptr(newMode))
	if r == 0 {
		return nil, fmt.Errorf("setconsolemode: %v", err)
	}
	return oldMode, nil
}

func restoreTerm(fd int, state interface{}) {
	if oldMode, ok := state.(uint32); ok {
		handle := syscall.Handle(fd)
		procSetConsoleMode.Call(uintptr(handle), uintptr(oldMode))
	}
}
