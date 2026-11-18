package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "shimamura-telemetry ") {
		t.Errorf("unexpected output: %q", stdout.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"unknown"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunNoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunAgentMissingConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "-config", "/nonexistent/config.toml"}, &stdout, &stderr); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}
