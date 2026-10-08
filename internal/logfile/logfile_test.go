package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatesBySizeAndKeepsLimit(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, "app.log", 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 20; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"app.log", "app.log.1", "app.log.2"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if info.Size() > 100 {
			t.Errorf("%s is %d bytes, over the limit", name, info.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "app.log.3")); !os.IsNotExist(err) {
		t.Errorf("more rotated files kept than configured")
	}
}

func TestAppendsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	w, _ := Open(dir, "app.log", 1<<20, 3)
	w.Write([]byte("one\n"))
	w.Close()
	w, _ = Open(dir, "app.log", 1<<20, 3)
	w.Write([]byte("two\n"))
	w.Close()
	b, _ := os.ReadFile(filepath.Join(dir, "app.log"))
	if string(b) != "one\ntwo\n" {
		t.Fatalf("restart must append, got %q", b)
	}
}
