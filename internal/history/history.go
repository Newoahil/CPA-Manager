// Package history is the append-only quota history store.
//
// Every successful ACTIVE collection cycle appends one line per credential per
// quota window. The file is JSON Lines: one JSON object per line, no trailing
// comma, UTF-8. Files are split by calendar day in the configured display zone
// (TZ_NAME): /data/history/quota-YYYY-MM-DD.jsonl.
//
// The field names and their order are a FROZEN CONTRACT for later pace
// prediction. A future reader may key off any of them; do not rename, reorder
// or repurpose a field, and do not drop a field that is merely absent on some
// lines. used_percent and reset_at are nullable: a window that never reported a
// usage number is skipped entirely (pace needs numbers), but the nullable shape
// is kept for windows that one day carry a reset without a percent.
//
// Record shape (in this order):
//
//	{"ts":"2026-10-09T09:00:00Z","key":"opaque-key","provider":"codex",
//	 "alias":"main","window":"账号 · 周窗口","window_seconds":604800,
//	 "used_percent":42.5,"reset_at":"2026-10-16T09:00:00Z","scope":"account",
//	 "source":"active"}
//
// The Writer is NOT safe for concurrent use. It is called only from the app's
// single scheduled cycle goroutine, which is also the single state writer.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// SourceActive marks a record produced by the scheduled active collection. The
// field exists so step 4's passive signals can be told apart later.
const SourceActive = "active"

const (
	filePrefix = "quota-"
	fileSuffix = ".jsonl"
	dayLayout  = "2006-01-02"
)

// Record is one window's reading at one cycle. Field order is part of the
// frozen contract; see the package comment.
type Record struct {
	TS            string   `json:"ts"`
	Key           string   `json:"key"`
	Provider      string   `json:"provider"`
	Alias         string   `json:"alias"`
	Window        string   `json:"window"`
	WindowSeconds int64    `json:"window_seconds"`
	UsedPercent   *float64 `json:"used_percent,omitempty"`
	ResetAt       string   `json:"reset_at,omitempty"`
	Scope         string   `json:"scope"`
	Source        string   `json:"source"`
}

// Writer appends quota records to day-split JSONL files. Not safe for
// concurrent use: it is owned by the app's single collection cycle.
type Writer struct {
	dir       string
	loc       *time.Location
	retention time.Duration

	day string // "YYYY-MM-DD" currently open, empty when no file is open
	f   *os.File
	buf *bufio.Writer
}

// NewWriter prepares the history directory and purges files past retention.
//
// loc is the zone whose midnight is the day boundary; it must be the configured
// display zone, never time.Local. A nil loc falls back to UTC so a caller
// mistake cannot silently pick the host's zone.
func NewWriter(dir string, loc *time.Location, retention time.Duration) (*Writer, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("history: empty directory")
	}
	if loc == nil {
		loc = time.UTC
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("history: create directory: %w", err)
	}
	w := &Writer{dir: dir, loc: loc, retention: retention}
	// Startup cleanup: only whole expired day-files are removed.
	w.purge(time.Now())
	return w, nil
}

// Dir returns the configured directory. It is used by callers that need to log
// where history is being written.
func (w *Writer) Dir() string { return w.dir }

// Append writes one record per successful snapshot window. Only snapshots with
// OK=true are recorded: a failed fetch has no trustworthy number, and a window
// with a nil UsedPercent is skipped because pace prediction needs a value.
//
// It is a no-op when there is nothing to record, so an ordinary failed cycle
// costs nothing.
func (w *Writer) Append(now time.Time, snaps []domain.QuotaSnapshot) error {
	records := buildRecords(now, snaps)
	if len(records) == 0 {
		return nil
	}
	day := now.In(w.loc).Format(dayLayout)
	if day != w.day {
		// Close the previous day and remove anything that aged out. A whole
		// file only ever holds one day, so deleting one loses no live data.
		if err := w.rotate(day, now); err != nil {
			return err
		}
	}
	if err := w.ensureOpen(); err != nil {
		return err
	}
	for i := range records {
		line, err := json.Marshal(&records[i])
		if err != nil {
			return fmt.Errorf("history: marshal record: %w", err)
		}
		if _, err := w.buf.Write(line); err != nil {
			return fmt.Errorf("history: write record: %w", err)
		}
		if err := w.buf.WriteByte('\n'); err != nil {
			return fmt.Errorf("history: write newline: %w", err)
		}
	}
	// Flush per cycle: the next append must not depend on a later cycle to
	// become durable, and the volume is small.
	if err := w.buf.Flush(); err != nil {
		return fmt.Errorf("history: flush: %w", err)
	}
	return nil
}

// Close flushes and closes the current day file. Safe to call more than once.
func (w *Writer) Close() error {
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.f != nil {
		err := w.f.Close()
		w.f, w.buf, w.day = nil, nil, ""
		return err
	}
	return nil
}

// rotate closes the open day file and purges expired ones.
func (w *Writer) rotate(day string, now time.Time) error {
	if err := w.Close(); err != nil {
		return err
	}
	w.purge(now)
	w.day = day
	return nil
}

// ensureOpen opens the current day's file for appending if it is not open.
func (w *Writer) ensureOpen() error {
	if w.f != nil {
		return nil
	}
	path := filepath.Join(w.dir, filePrefix+w.day+fileSuffix)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("history: open %s: %w", path, err)
	}
	w.f = f
	w.buf = bufio.NewWriter(f)
	return nil
}

// purge removes day files whose date is at or past the retention cutoff. It
// parses the date from the file NAME, not the mtime: a copied or restored file
// must expire on its recorded day regardless of when it landed on disk. Files
// that do not match the quota-YYYY-MM-DD.jsonl shape are left untouched.
func (w *Writer) purge(now time.Time) {
	if w.retention <= 0 {
		return
	}
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		day, ok := w.parseDay(e.Name())
		if !ok {
			continue
		}
		// A day file expires exactly retention after its own date, keeping the
		// newest retention days.
		if !day.Add(w.retention).After(now) {
			_ = os.Remove(filepath.Join(w.dir, e.Name()))
		}
	}
}

// parseDay recovers the day a file records from its name.
func (w *Writer) parseDay(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
		return time.Time{}, false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
	t, err := time.ParseInLocation(dayLayout, raw, w.loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// buildRecords projects snapshots onto records. It reads only successful
// snapshots and only windows that reported a number.
func buildRecords(now time.Time, snaps []domain.QuotaSnapshot) []Record {
	ts := now.UTC().Format(time.RFC3339)
	var out []Record
	for _, s := range snaps {
		if !s.OK {
			continue
		}
		alias := domain.TrimProviderPrefix(strings.TrimSpace(s.Credential.Alias), s.Credential.Provider)
		for _, w := range s.Windows {
			if w.UsedPercent == nil {
				continue
			}
			used := *w.UsedPercent
			rec := Record{
				TS:            ts,
				Key:           s.Credential.Key,
				Provider:      string(s.Credential.Provider),
				Alias:         alias,
				Window:        w.DisplayLabel(),
				WindowSeconds: w.WindowSeconds,
				UsedPercent:   &used,
				Scope:         string(w.Scope.Normalized()),
				Source:        SourceActive,
			}
			if w.ResetAt != nil {
				rec.ResetAt = w.ResetAt.UTC().Format(time.RFC3339)
			}
			out = append(out, rec)
		}
	}
	return out
}

// Read parses every record in one JSONL file. It exists for tests and for the
// pace-prediction reader that will land next; malformed lines are an error so a
// corrupt file is never silently half-read.
func Read(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("history: parse %s: %w", path, err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
