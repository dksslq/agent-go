//go:build darwin
// +build darwin

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

func makeRaw(fd int) (interface{}, error) {
	var oldState syscall.Termios
	if _, _, err := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGETA, uintptr(unsafe.Pointer(&oldState)), 0, 0, 0); err != 0 {
		return nil, fmt.Errorf("tcgetattr: %v", err)
	}
	newState := oldState
	newState.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.IXON
	newState.Oflag &^= syscall.OPOST
	newState.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.IEXTEN
	newState.Cflag &^= syscall.CSIZE | syscall.PARENB
	newState.Cflag |= syscall.CS8
	newState.Cc[syscall.VMIN] = 1
	newState.Cc[syscall.VTIME] = 0
	if _, _, err := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSETA, uintptr(unsafe.Pointer(&newState)), 0, 0, 0); err != 0 {
		return nil, fmt.Errorf("tcsetattr: %v", err)
	}
	return &oldState, nil
}

func restoreTerm(fd int, state interface{}) {
	if s, ok := state.(*syscall.Termios); ok {
		syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSETA, uintptr(unsafe.Pointer(s)), 0, 0, 0)
	}
}
