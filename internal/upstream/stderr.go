package upstream

import (
	"bytes"
	"log/slog"
	"sync"
)

// maxStderrLine caps a single logged stderr line so a chatty or binary
// upstream can't grow the buffer without bound.
const maxStderrLine = 4096

// lineLogger is an io.Writer that logs each complete line written to it.
type lineLogger struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

func newLineLogger(log *slog.Logger) *lineLogger {
	return &lineLogger{log: log}
}

func (l *lineLogger) Write(p []byte) (int, error) {
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
	for len(l.buf) >= maxStderrLine {
		l.emit(l.buf[:maxStderrLine])
		l.buf = l.buf[maxStderrLine:]
	}
	if len(l.buf) == 0 {
		l.buf = nil // drop the backing array between lines
	}
	return len(p), nil
}

// Flush logs any trailing partial line.
func (l *lineLogger) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.emit(l.buf)
		l.buf = nil
	}
}

func (l *lineLogger) emit(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if len(line) > maxStderrLine {
		line = line[:maxStderrLine]
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	l.log.Info("upstream stderr", "line", string(line))
}
