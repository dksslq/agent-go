//go:build windows
// +build windows

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

type consoleState struct {
	input, output uint32
	outputHandle  syscall.Handle
}

func makeRaw(fd int) (interface{}, error) {
	state := consoleState{outputHandle: syscall.Handle(os.Stdout.Fd())}
	r, _, err := procGetConsoleMode.Call(uintptr(fd), uintptr(unsafe.Pointer(&state.input)))
	if r == 0 {
		return nil, fmt.Errorf("getconsolemode: %v", err)
	}
	r, _, err = procGetConsoleMode.Call(uintptr(state.outputHandle), uintptr(unsafe.Pointer(&state.output)))
	if r == 0 {
		return nil, fmt.Errorf("getconsolemode output: %v", err)
	}
	r, _, err = procSetConsoleMode.Call(uintptr(state.outputHandle), uintptr(state.output|0x0001|0x0004))
	if r == 0 {
		return nil, fmt.Errorf("setconsolemode output: %v", err)
	}
	// Keep Ctrl+C signals; enable VT key and bracketed-paste sequences.
	mode := (state.input &^ (0x0002 | 0x0004)) | 0x0001 | 0x0200
	r, _, err = procSetConsoleMode.Call(uintptr(fd), uintptr(mode))
	if r == 0 {
		procSetConsoleMode.Call(uintptr(state.outputHandle), uintptr(state.output))
		return nil, fmt.Errorf("setconsolemode: %v", err)
	}
	return state, nil
}

func restoreTerm(fd int, old interface{}) {
	if state, ok := old.(consoleState); ok {
		procSetConsoleMode.Call(uintptr(fd), uintptr(state.input))
		procSetConsoleMode.Call(uintptr(state.outputHandle), uintptr(state.output))
	}
}
