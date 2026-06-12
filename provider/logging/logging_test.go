package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestSetAndUseLogger(t *testing.T) {
	orig := L()
	t.Cleanup(func() { SetLogger(orig) })

	var buf bytes.Buffer
	SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	L().Info("hello", "key", "value")

	out := buf.String()
	if !strings.Contains(out, "hello") || !strings.Contains(out, "key=value") {
		t.Fatalf("log output missing fields: %q", out)
	}
}

func TestSetLoggerIgnoresNil(t *testing.T) {
	orig := L()
	t.Cleanup(func() { SetLogger(orig) })

	var buf bytes.Buffer
	custom := slog.New(slog.NewTextHandler(&buf, nil))
	SetLogger(custom)
	SetLogger(nil) // must be a no-op, not panic / reset

	if L() != custom {
		t.Fatal("SetLogger(nil) should not replace the existing logger")
	}
}
