//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// No process lock: only explicitly selected files are opened.
type sessionOwner struct {
	path    string
	file    *os.File
	created bool
}

func reserveSession(path string, create bool) (*sessionOwner, error) {
	if path == "" {
		return nil, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("session must be a regular file")
	}
	f, err := os.OpenFile(absolute, os.O_RDWR, 0600)
	created := false
	if err != nil && create && errors.Is(err, os.ErrNotExist) {
		f, err = os.OpenFile(absolute, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		created = err == nil
	}
	if err != nil {
		return nil, err
	}
	return &sessionOwner{path: absolute, file: f, created: created}, nil
}
func (s *sessionOwner) matches(path string) bool {
	if s == nil || s.file == nil || path == "" {
		return false
	}
	absolute, err := filepath.Abs(path)
	return err == nil && s.path == absolute
}
func (s *sessionOwner) createdNew() bool { return s != nil && s.file != nil && s.created }
func (*sessionOwner) exclusive() bool    { return false }
func (s *sessionOwner) close() error {
	if s == nil || s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	if s.created {
		err = errors.Join(err, os.Remove(s.path))
		s.created = false
	}
	return err
}

func syncSessionDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	err = dir.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}
