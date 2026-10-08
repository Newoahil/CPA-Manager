// Package logfile keeps a copy of the process log on the persistent /data
// volume. Container log drivers (as configured on Dokploy) keep only a short
// tail, which is not enough to find out afterwards why an alert did or did not
// fire. Files rotate by size and only the newest few are kept, so the volume
// cannot fill up.
//
// The log never contains secrets: every component logs scrubbed fields only,
// and this package adds nothing of its own.
package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Writer is an io.Writer that appends to dir/name and rotates it once it
// would exceed maxBytes, keeping at most keep rotated files.
type Writer struct {
	mu       sync.Mutex
	dir      string
	name     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

// Open creates dir if needed and opens the current log file for appending.
func Open(dir, name string, maxBytes int64, keep int) (*Writer, error) {
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	if keep < 1 {
		keep = 1
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logfile: create %s: %w", dir, err)
	}
	w := &Writer{dir: dir, name: name, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) path() string { return filepath.Join(w.dir, w.name) }

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logfile: open: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("logfile: stat: %w", err)
	}
	w.f, w.size = f, info.Size()
	return nil
}

// Write appends p, rotating first when p would push the file past maxBytes.
// A failed rotation keeps writing to the current file: losing rotation is
// better than losing log lines.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		_ = w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate renames the current file to name.<n> (1 = newest) and opens a fresh
// one, dropping files beyond keep.
func (w *Writer) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil
	for i := w.keep; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path(), i)
		if i == w.keep {
			_ = os.Remove(src)
			continue
		}
		_ = os.Rename(src, fmt.Sprintf("%s.%d", w.path(), i+1))
	}
	_ = os.Rename(w.path(), w.path()+".1")
	w.cleanup()
	return w.open()
}

// cleanup removes stray rotated files beyond keep (e.g. after keep shrank).
func (w *Writer) cleanup() {
	matches, _ := filepath.Glob(w.path() + ".*")
	sort.Strings(matches)
	for _, m := range matches {
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(m, w.path()+"."), "%d", &n); err == nil && n > w.keep {
			_ = os.Remove(m)
		}
	}
}

// Close closes the current file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
