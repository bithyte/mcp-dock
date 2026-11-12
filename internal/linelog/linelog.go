// Package linelog turns a byte stream (a child's stderr) into log records,
// one per line.
package linelog

import (
	"bytes"
	"log/slog"
	"sync"
)

// maxLine caps a single logged line so a chatty or binary child can't grow
// the buffer without bound.
const maxLine = 4096

// Writer is an io.Writer that logs each complete line written to it.
type Writer struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

// New returns a Writer logging to log.
func New(log *slog.Logger) *Writer {
	return &Writer{log: log}
}

func (l *Writer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.emit(l.buf[:i])
		l.buf = l.buf[i+1:]
	}
	for len(l.buf) >= maxLine {
		l.emit(l.buf[:maxLine])
		l.buf = l.buf[maxLine:]
	}
	if len(l.buf) == 0 {
		l.buf = nil // drop the backing array between lines
	}
	return len(p), nil
}

// Flush logs any trailing partial line.
func (l *Writer) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.emit(l.buf)
		l.buf = nil
	}
}

func (l *Writer) emit(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if len(line) > maxLine {
		line = line[:maxLine]
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	l.log.Info("upstream stderr", "line", string(line))
}
