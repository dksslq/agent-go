package main

import (
	"context"
	"os"
)

// Inherited stdout/stderr may be blocking descriptors. Keep exactly one
// writer per stream, so a consumer that stops reading cannot trap shutdown
// or accumulate one blocked goroutine per token/tool. Process exit releases
// a writer stuck in an OS write; no queued/unbounded output buffer is used.
type outputWriter struct {
	ctx  context.Context
	jobs chan outputJob
}

type outputJob struct {
	text string
	done chan error
}

func newOutputWriter(ctx context.Context, file *os.File) *outputWriter {
	w := &outputWriter{ctx: ctx, jobs: make(chan outputJob)}
	go func() {
		for {
			select {
			case job := <-w.jobs:
				_, err := file.WriteString(job.text)
				job.done <- err
			case <-ctx.Done():
				return
			}
		}
	}()
	return w
}

func (w *outputWriter) write(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	job := outputJob{text: text, done: make(chan error, 1)}
	select {
	case w.jobs <- job:
	case <-ctx.Done():
		return ctx.Err()
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
	select {
	case err := <-job.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

var stdoutWriter, stderrWriter *outputWriter
