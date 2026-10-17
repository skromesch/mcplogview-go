// Package serverlog is the file-based self-log with size rotation. It never
// writes to stdout/stderr: stdout is reserved for MCP JSON-RPC in stdio mode.
package serverlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Logger is the logging surface used across the server.
type Logger interface {
	Info(msg string)
	Error(msg string, err error)
	Close()
}

type noop struct{}

func (noop) Info(string)         {}
func (noop) Error(string, error) {}
func (noop) Close()              {}

// Noop returns a logger that discards everything.
func Noop() Logger { return noop{} }

type fileLogger struct {
	mu          sync.Mutex
	path        string
	maxFileSize int64
	maxBackups  int
}

// New creates the log directory and returns a rotating file logger.
func New(path string, maxFileSize int64, maxBackups int) (Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if maxFileSize <= 0 {
		maxFileSize = 10 * 1024 * 1024
	}
	if maxBackups <= 0 {
		maxBackups = 5
	}
	return &fileLogger{path: path, maxFileSize: maxFileSize, maxBackups: maxBackups}, nil
}

func (l *fileLogger) Info(msg string) { l.write("INFO", msg) }

func (l *fileLogger) Error(msg string, err error) {
	details := "unknown"
	if err != nil {
		details = err.Error()
	}
	l.write("ERROR", msg+" | "+details)
}

func (l *fileLogger) Close() { l.write("INFO", "server shutting down") }

func (l *fileLogger) write(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	line := fmt.Sprintf("%s [%s] %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), level, strings.TrimRight(msg, "\n"))
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()

	if st, err := os.Stat(l.path); err == nil && st.Size() >= l.maxFileSize {
		l.rotate()
	}
}

// rotate shifts file.N -> file.N+1 (dropping the oldest) and file -> file.1.
func (l *fileLogger) rotate() {
	for i := l.maxBackups; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", l.path, i)
		if _, err := os.Stat(old); err != nil {
			continue
		}
		if i+1 > l.maxBackups {
			_ = os.Remove(old)
		} else {
			_ = os.Rename(old, fmt.Sprintf("%s.%d", l.path, i+1))
		}
	}
	_ = os.Rename(l.path, l.path+".1")
}
