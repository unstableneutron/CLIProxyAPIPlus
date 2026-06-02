package registry

import (
	"bytes"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestValidateModelSectionSkipsWarningForCommandCodeFallback(t *testing.T) {
	var buf bytes.Buffer
	logger := log.StandardLogger()
	oldOut := logger.Out
	log.SetOutput(&buf)
	defer log.SetOutput(oldOut)

	if err := validateModelSection("commandcode", nil); err != nil {
		t.Fatalf("validateModelSection returned error: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "commandcode section is empty") {
		t.Fatalf("expected no warning for commandcode fallback, got %q", got)
	}
}

func TestValidateModelSectionWarnsForEmptySectionWithoutFallback(t *testing.T) {
	var buf bytes.Buffer
	logger := log.StandardLogger()
	oldOut := logger.Out
	log.SetOutput(&buf)
	defer log.SetOutput(oldOut)

	if err := validateModelSection("claude", nil); err != nil {
		t.Fatalf("validateModelSection returned error: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "models catalog: claude section is empty") {
		t.Fatalf("expected warning for empty claude section, got %q", got)
	}
}
