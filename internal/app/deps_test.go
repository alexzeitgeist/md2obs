package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestDepsLoggerIsInitializedOnce(t *testing.T) {
	var output bytes.Buffer
	deps := &Deps{Err: &output}

	first := deps.logger()
	second := deps.logger()
	if first != second {
		t.Fatal("logger() constructed more than one fallback logger")
	}

	first.Info("test message")
	if !strings.Contains(output.String(), "test message") {
		t.Fatalf("fallback logger did not use Err: %q", output.String())
	}
}
