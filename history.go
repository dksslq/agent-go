package main

import (
	"context"
	"errors"
	"fmt"
	"os"
)

type historyMessage struct {
	Index   int     `json:"index"`
	Message Message `json:"message"`
}

type historyReadResult struct {
	Start    int              `json:"start"`
	End      int              `json:"end"`
	Next     int              `json:"next"`
	Base     int              `json:"base"`
	Messages []historyMessage `json:"messages"`
}

// Only the requested originals enter the tool response. Holding the session
// lock also protects a borrowed Windows-exclusive handle from close or append.
func (s *Session) readHistory(ctx context.Context, start, end int) (result historyReadResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.historyPath == "" {
		return result, errors.New("no saved history selected; use /save, /load or /continue first")
	}
	if start < 0 || end < start || end > s.historyCount {
		return result, fmt.Errorf("history range must satisfy 0 <= start <= end <= %d", s.historyCount)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	file := s.owner
	var source *os.File
	if file.matches(s.historyPath) {
		source = file.file
	} else {
		source, err = os.Open(s.historyPath)
		if err != nil {
			return result, fmt.Errorf("open saved history: %w", err)
		}
		defer func() { err = errors.Join(err, source.Close()) }()
	}
	result = historyReadResult{Start: start, End: end, Next: s.historyCount, Messages: make([]historyMessage, 0)}
	state, err := readSessionLog(source, func(index int, message Message) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if index >= start && index < end {
			result.Messages = append(result.Messages, historyMessage{Index: index, Message: message})
		}
		return nil
	})
	if err != nil {
		return historyReadResult{}, fmt.Errorf("read saved history: %w", err)
	}
	if state.Count < s.historyCount {
		return historyReadResult{}, errors.New("saved history no longer contains the selected checkpoint")
	}
	result.Base = state.Base
	if len(result.Messages) != end-start {
		if state.Base > 0 && start <= state.Base && end > 1 {
			return historyReadResult{}, fmt.Errorf("original messages [1,%d) were discarded before saving and cannot be recovered", state.Base+1)
		}
		return historyReadResult{}, errors.New("saved history no longer contains the selected range")
	}
	return result, nil
}
