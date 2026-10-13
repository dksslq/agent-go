//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

// Unix keeps its normal atomic-replace semantics without an ownership gate.
type sessionOwner struct{ file *os.File }

func reserveSession(string, bool) (*sessionOwner, error) { return nil, nil }
func (*sessionOwner) matches(string) bool                { return false }
func (*sessionOwner) close() error                       { return nil }
func (*sessionOwner) createdNew() bool                   { return false }

func createSessionTemp(path string) (*os.File, error) {
	return os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
}

func commitSession(path string, f *os.File, _ *sessionOwner) error {
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
