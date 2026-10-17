package serverlog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritesAndRotates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "s.log")
	l, err := New(p, 1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello")
	l.Error("boom", errors.New("bad"))
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "[INFO] hello") || !strings.Contains(string(b), "[ERROR] boom | bad") {
		t.Fatalf("%s", b)
	}

	for i := 0; i < 200; i++ {
		l.Info(strings.Repeat("x", 100))
	}
	for _, f := range []string{p + ".1", p + ".2"} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("missing backup %s", f)
		}
	}
	if _, err := os.Stat(p + ".3"); err == nil {
		t.Fatal("maxBackups exceeded")
	}
}
