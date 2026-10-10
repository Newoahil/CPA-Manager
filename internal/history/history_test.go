package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func shanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

func ptr(v float64) *float64 { return &v }

func snap(key string, provider domain.ProviderKind, alias string, windows ...domain.QuotaWindow) domain.QuotaSnapshot {
	return domain.QuotaSnapshot{
		Credential: domain.Credential{Key: key, Provider: provider, Alias: alias},
		Windows:    windows, OK: true,
	}
}

// TestWriterCreatesDirAndFields: the directory is created and every field the
// pace-prediction contract names is written, including a trimmed alias and the
// nullable used/reset.
func TestWriterCreatesDirAndFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	loc := shanghai(t)
	w, err := NewWriter(dir, loc, 2160*time.Hour)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	reset := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC) // 17:00 Shanghai
	ws := []domain.QuotaWindow{
		{Name: "ollama/five_hour", Label: "账号 · 5小时", Scope: domain.ScopeAccount, WindowSeconds: 18000, UsedPercent: ptr(23.7), ResetAt: &reset},
		{Name: "ollama/seven_day", Label: "账号 · 7天", Scope: domain.ScopeAccount, WindowSeconds: 604800, UsedPercent: ptr(33.8)},
	}
	if err := w.Append(now, []domain.QuotaSnapshot{snap("k1", domain.ProviderOllama, "ollama-main", ws...)}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("history dir not created: %v", err)
	}

	path := filepath.Join(dir, "quota-2026-10-09.jsonl")
	recs, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	got := recs[0]
	if got.TS != "2026-10-09T09:00:00Z" || got.Key != "k1" || got.Provider != "ollama" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.Alias != "main" {
		t.Errorf("alias = %q, want main (provider prefix trimmed)", got.Alias)
	}
	if got.Window != "账号 · 5小时" || got.WindowSeconds != 18000 {
		t.Errorf("window fields wrong: %+v", got)
	}
	if got.UsedPercent == nil || *got.UsedPercent != 23.7 {
		t.Errorf("used_percent wrong: %+v", got.UsedPercent)
	}
	if got.ResetAt != "2026-10-09T18:00:00Z" {
		t.Errorf("reset_at = %q", got.ResetAt)
	}
	if got.Scope != "account" || got.Source != "active" {
		t.Errorf("scope/source = %q/%q", got.Scope, got.Source)
	}
	// Second window has no reset: the field must be omitted, not empty-quoted.
	if recs[1].ResetAt != "" {
		t.Errorf("reset-less window carried reset_at %q", recs[1].ResetAt)
	}
	// used_percent is nullable and omitted only when nil.
	raw, _ := os.ReadFile(path)
	if strings.Count(strings.TrimSpace(string(raw)), "\n") != 1 {
		t.Errorf("expected one JSON object per line:\n%s", raw)
	}
	if json.Valid(raw) {
		t.Error("a multi-line JSONL file must not itself be a single JSON value")
	}
}

// TestAppendSkipsNoNumberAndFailures: only successful snapshots with a reported
// number are recorded.
func TestAppendSkipsNoNumberAndFailures(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, time.UTC, 2160*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	failed := domain.QuotaSnapshot{Credential: domain.Credential{Key: "bad", Provider: domain.ProviderCodex}, Windows: []domain.QuotaWindow{{Name: "5h", UsedPercent: ptr(50)}}, OK: false}
	noNum := snap("k", domain.ProviderCodex, "x", domain.QuotaWindow{Name: "5h"})
	if err := w.Append(now, []domain.QuotaSnapshot{failed, noNum}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "quota-2026-10-09.jsonl")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		// Nothing to record means no file (or an empty one). Either is fine, but
		// no record may exist.
		if recs, rerr := Read(path); rerr == nil && len(recs) != 0 {
			t.Fatalf("recorded skipped snapshots: %+v", recs)
		}
	}
}

// TestDayBoundaryUsesConfiguredZone: 23:59 Shanghai and 00:01 Shanghai land in
// different files, and a UTC clock on the same instant would not.
func TestDayBoundaryUsesConfiguredZone(t *testing.T) {
	dir := t.TempDir()
	loc := shanghai(t)
	w, err := NewWriter(dir, loc, 2160*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// 2026-10-09 15:59 UTC == 23:59 Shanghai (still the 9th).
	before := time.Date(2026, 10, 9, 15, 59, 0, 0, time.UTC)
	// 2026-10-09 16:01 UTC == 00:01 Shanghai on the 10th.
	after := time.Date(2026, 10, 9, 16, 1, 0, 0, time.UTC)
	s := snap("k", domain.ProviderCodex, "main", domain.QuotaWindow{Name: "5h", UsedPercent: ptr(10)})
	if err := w.Append(before, []domain.QuotaSnapshot{s}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(after, []domain.QuotaSnapshot{s}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "quota-2026-10-09.jsonl")); err != nil {
		t.Errorf("before-midnight file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "quota-2026-10-10.jsonl")); err != nil {
		t.Errorf("after-midnight file missing: %v", err)
	}
}

// TestReopenAppendAndReadBack: a second writer on the same directory appends to
// the existing file rather than truncating it.
func TestReopenAppendAndReadBack(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	s := snap("k", domain.ProviderCodex, "main", domain.QuotaWindow{Name: "5h", UsedPercent: ptr(10)})

	w1, err := NewWriter(dir, time.UTC, 2160*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := w1.Append(now, []domain.QuotaSnapshot{s}); err != nil {
		t.Fatal(err)
	}
	if err := w1.Close(); err != nil {
		t.Fatal(err)
	}
	w2, err := NewWriter(dir, time.UTC, 2160*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Append(now.Add(time.Minute), []domain.QuotaSnapshot{s}); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	recs, err := Read(filepath.Join(dir, "quota-2026-10-09.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2 (append across reopen)", len(recs))
	}
}

// TestRetentionDeletesOnlyExpiredDayFiles: purge keys off the file NAME date,
// leaves unparsable names alone, and never touches other files.
func TestRetentionDeletesOnlyExpiredDayFiles(t *testing.T) {
	dir := t.TempDir()
	// now is 2026-10-09; retention 90 days expires anything before 2026-07-11.
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	retention := 90 * 24 * time.Hour
	must := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("quota-2026-10-09.jsonl") // today, keep
	must("quota-2026-08-01.jsonl") // within 90d, keep
	must("quota-2026-07-10.jsonl") // 91 days old, delete (name-date, not mtime)
	must("quota-2025-01-01.jsonl") // ancient, delete
	must("keep-me.txt")            // unrelated, leave
	must("quota-not-a-day.jsonl")  // unparsable, leave

	w, err := NewWriter(dir, time.UTC, retention)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// NewWriter purges on startup.
	if err := w.Append(now, []domain.QuotaSnapshot{snap("k", domain.ProviderCodex, "main", domain.QuotaWindow{Name: "5h", UsedPercent: ptr(10)})}); err != nil {
		// Append purges again at the (absent) rollover; either way this is fine.
		t.Fatal(err)
	}

	for _, name := range []string{"quota-2026-10-09.jsonl", "quota-2026-08-01.jsonl", "keep-me.txt", "quota-not-a-day.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should survive purge: %v", name, err)
		}
	}
	for _, name := range []string{"quota-2026-07-10.jsonl", "quota-2025-01-01.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should have been purged", name)
		}
	}
}

// TestRecordFieldOrderIsFrozen: a future pace reader keys off the field order,
// so the JSON key order is asserted explicitly.
func TestRecordFieldOrderIsFrozen(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, time.UTC, 2160*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	reset := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	if err := w.Append(now, []domain.QuotaSnapshot{snap("k", domain.ProviderCodex, "codex-main",
		domain.QuotaWindow{Name: "5h", Label: "账号 · 5小时", Scope: domain.ScopeAccount, WindowSeconds: 18000, UsedPercent: ptr(42.5), ResetAt: &reset})}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "quota-2026-10-09.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(raw))
	wantOrder := []string{"ts", "key", "provider", "alias", "window", "window_seconds", "used_percent", "reset_at", "scope", "source"}
	pos := 0
	for _, key := range wantOrder {
		idx := strings.Index(line, `"`+key+`"`)
		if idx < 0 {
			t.Fatalf("field %q missing from %s", key, line)
		}
		if idx < pos {
			t.Fatalf("field %q out of the frozen order in %s", key, line)
		}
		pos = idx
	}
}
