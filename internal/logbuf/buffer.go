// Package logbuf keeps the recent process log so the configuration page can
// show it. The lines still go to the process output as well.
package logbuf

import (
	"bytes"
	"sync"
)

const maxLines = 400

var shared = &buffer{max: maxLines}

type buffer struct {
	mu      sync.Mutex
	stored  []string
	pending []byte
	max     int
}

// Writer is the process log. Install it with log.SetOutput alongside stderr.
func Writer() *buffer {
	return shared
}

// Lines returns a copy of the recent log, oldest first.
func Lines() []string {
	return shared.lines()
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, p...)
	for {
		i := bytes.IndexByte(b.pending, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(b.pending[:i], "\r"))
		b.pending = append([]byte(nil), b.pending[i+1:]...)
		if line == "" {
			continue
		}
		if len(line) > 4000 {
			line = line[:4000]
		}
		b.stored = append(b.stored, line)
		if len(b.stored) > b.max {
			b.stored = append([]string(nil), b.stored[len(b.stored)-b.max:]...)
		}
	}
	if len(b.pending) > 8192 {
		b.pending = append([]byte(nil), b.pending[len(b.pending)-8192:]...)
	}
	return len(p), nil
}

func (b *buffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.stored) == 0 {
		return []string{}
	}
	out := make([]string, len(b.stored))
	copy(out, b.stored)
	return out
}
