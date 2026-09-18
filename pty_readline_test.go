//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY 创建真实伪终端对（/dev/ptmx + /dev/pts/N），用于在 raw 模式下实测 readLine。
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	mfd, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	var ptn int
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(mfd), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptn))); e != 0 {
		syscall.Close(mfd)
		t.Fatalf("TIOCGPTN: %v", e)
	}
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(mfd), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		syscall.Close(mfd)
		t.Fatalf("TIOCSPTLCK: %v", e)
	}
	sfd, err := syscall.Open(fmt.Sprintf("/dev/pts/%d", ptn), syscall.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		syscall.Close(mfd)
		t.Fatalf("open slave: %v", err)
	}
	return os.NewFile(uintptr(mfd), "/dev/ptmx"), os.NewFile(uintptr(sfd), "/dev/pts")
}

// runRawLine 在真实 PTY 的 slave 端以 raw 模式运行 readLine，master 端写入按键字节，返回读到的行。
func runRawLine(t *testing.T, input []byte) string {
	t.Helper()
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	st, err := makeRaw(int(slave.Fd()))
	if err != nil {
		t.Fatalf("makeRaw: %v", err)
	}
	defer restoreTerm(int(slave.Fd()), st)

	oldRaw, oldFd, oldState := termRaw, termFd, termOld
	termRaw, termFd, termOld = true, int(slave.Fd()), st
	defer func() { termRaw, termFd, termOld = oldRaw, oldFd, oldState }()

	if _, err := master.Write(input); err != nil {
		t.Fatalf("write master: %v", err)
	}
	line, err := readLine(bufio.NewReader(slave))
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	return line
}

// 核心实测：方向键 ↑(ESC[A) / Home(ESC[H) / Delete(ESC[3~) 之后输入 "ok" 回车。
// 期望：转义序列被完整忽略，读到的行是 "ok"。
func TestReadLineArrowKeys(t *testing.T) {
	got := runRawLine(t, []byte("\x1b[A\x1b[H\x1b[3~ok\r"))
	if got != "ok" {
		t.Fatalf("确认问题：方向键转义序列污染输入，got %q, want %q", got, "ok")
	}
	t.Logf("输入行 = %q（转义序列未混入）", got)
}

// 回归：普通文本输入不受影响。
func TestReadLinePlainText(t *testing.T) {
	got := runRawLine(t, []byte("hello\r"))
	if got != "hello" {
		t.Fatalf("plain input broken: got %q", got)
	}
}

// 回归：退格删除仍然有效（ab + BS → a，再输入 c → ac）。
func TestReadLineBackspace(t *testing.T) {
	got := runRawLine(t, []byte("ab\x7fc\r"))
	if got != "ac" {
		t.Fatalf("backspace broken: got %q, want %q", got, "ac")
	}
}
