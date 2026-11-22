// Package debuglog writes one directory per request under
// <state>/debug/<ts>-<id>/ so a failing turn can be replayed from artifacts.
// It is deliberately file-based (no DB) — a single-tenant local proxy does
// not need devin-2api's SQLite log plane.
package debuglog

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Logger appends to per-request debug dirs.
type Logger struct {
	Root      string // <state>/debug
	Retention time.Duration
	LogFrames bool

	mu sync.Mutex
}

// New prepares the logger and sweeps expired dirs once.
func New(stateDir string, enabled bool, retentionDays int, logFrames bool) (*Logger, error) {
	if !enabled {
		return &Logger{}, nil
	}
	root := filepath.Join(stateDir, "debug")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	l := &Logger{Root: root, LogFrames: logFrames}
	if retentionDays > 0 {
		l.Retention = time.Duration(retentionDays) * 24 * time.Hour
		l.sweep()
	}
	return l, nil
}

// Enabled reports whether logging is active.
func (l *Logger) Enabled() bool { return l != nil && l.Root != "" }

// Dir creates and returns the directory for one request.
func (l *Logger) Dir(reqID string) string {
	if !l.Enabled() {
		return ""
	}
	dir := filepath.Join(l.Root, time.Now().UTC().Format("20060102-150405.000")+"-"+sanitizeID(reqID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("debuglog mkdir failed", "error", err)
		return ""
	}
	return dir
}

// Write appends one JSON record to <dir>/<name>.jsonl (or writes it whole
// when the name already ends in .json).
func (l *Logger) Write(dir, name string, v any) {
	if dir == "" || v == nil {
		return
	}
	path := filepath.Join(dir, name)
	if !strings.HasSuffix(path, ".json") && !strings.HasSuffix(path, ".jsonl") {
		path += ".jsonl"
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// sweep removes debug dirs older than the retention window. Dir names are
// prefixed with their creation timestamp, so age is the file mtime.
func (l *Logger) sweep() {
	if l.Root == "" || l.Retention <= 0 {
		return
	}
	cutoff := time.Now().Add(-l.Retention)
	entries, err := os.ReadDir(l.Root)
	if err != nil {
		return
	}
	var stale []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			stale = append(stale, filepath.Join(l.Root, e.Name()))
		}
	}
	sort.Strings(stale)
	for _, p := range stale {
		if err := os.RemoveAll(p); err != nil {
			slog.Warn("debuglog sweep failed", "path", p, "error", err)
		}
	}
	if len(stale) > 0 {
		slog.Info("debuglog swept", "removed", len(stale))
	}
}

// sanitizeID strips anything that would break a directory name.
func sanitizeID(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, s)
}
