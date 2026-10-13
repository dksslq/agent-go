//go:build windows

package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// No read/write sharing: competing opens fail immediately. DELETE sharing is
// necessary for replacement while both snapshot handles remain held;
// other programs with deletion rights can still rename/delete the pathname.
type sessionOwner struct {
	path    string
	file    *os.File
	created bool
}

var sessionDisposition = syscall.NewLazyDLL("kernel32.dll").NewProc("SetFileInformationByHandle")

func reserveSession(path string, create bool) (*sessionOwner, error) {
	if path == "" {
		return nil, nil
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := openSessionFile(path, syscall.OPEN_EXISTING)
	created := false
	if err != nil && create && errors.Is(err, os.ErrNotExist) {
		f, err = openSessionFile(path, syscall.CREATE_NEW)
		created = err == nil // a competing creator fails; never reopen/retry
	}
	if err != nil {
		return nil, err
	}
	return &sessionOwner{path: path, file: f, created: created}, nil
}

func (s *sessionOwner) createdNew() bool { return s != nil && s.created }

func (s *sessionOwner) matches(path string) bool {
	if s == nil {
		return false
	}
	absolute, err := filepath.Abs(path)
	return err == nil && strings.EqualFold(s.path, absolute)
}

func (s *sessionOwner) close() error {
	if s != nil && s.file != nil {
		var cleanupErr error
		if s.created {
			// FileDispositionInfo deletes this owned placeholder, not whichever
			// object an outside renamer may now have placed at the same pathname.
			var deleteFile byte = 1
			if ok, _, err := sessionDisposition.Call(s.file.Fd(), 4, uintptr(unsafe.Pointer(&deleteFile)), 1); ok == 0 {
				cleanupErr = &os.PathError{Op: "remove unused session", Path: s.path, Err: err}
			}
		}
		closeErr := s.file.Close()
		s.file = nil
		return errors.Join(cleanupErr, closeErr)
	}
	return nil
}

func openSessionFile(path string, mode uint32) (*os.File, error) {
	// Native CreateFile needs the extended prefix for ordinary long paths too.
	if !strings.HasPrefix(path, `\\?\`) {
		if strings.HasPrefix(path, `\\`) {
			path = `\\?\UNC\` + path[2:]
		} else {
			path = `\\?\` + path
		}
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	const deleteAccess = 0x00010000 // Win32 DELETE access for owned-placeholder cleanup
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE|deleteAccess, syscall.FILE_SHARE_DELETE, nil, mode, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "reserve session", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(handle), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = fmt.Errorf("session must be a regular file")
		}
		return nil, err
	}
	return f, nil
}

func createSessionTemp(path string) (*os.File, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	name := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%x.tmp", filepath.Base(path), nonce))
	return openSessionFile(name, syscall.CREATE_NEW)
}

func commitSession(path string, f *os.File, owner *sessionOwner) error {
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	old := owner.file
	owner.file = f // new target was already reserved before replacement
	owner.created = false
	old.Close()
	return nil
}
