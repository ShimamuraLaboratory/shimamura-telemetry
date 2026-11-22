package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestRunCollect(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"collect"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	var got struct {
		MeasuredAt int64 `json:"measuredAt"`
		Metrics    struct {
			CPU    map[string]any `json:"cpu"`
			Memory map[string]any `json:"memory"`
			Disks  []any          `json:"disks"`
			GPUs   []any          `json:"gpus"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if got.MeasuredAt <= 0 || got.Metrics.CPU == nil || got.Metrics.Memory == nil || got.Metrics.GPUs == nil {
		t.Errorf("unexpected output: %s", stdout.String())
	}
}

func TestRunCheckConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	valid := write("valid.toml", `server_id = "s"
agent_token = "t"
url = "wss://example.com/agent/connect"
`)
	// IdP の古いテンプレートの形式（キー名が違う）
	unknown := write("unknown.toml", `server_id = "s"
token = "t"
endpoint = "wss://example.com/agent/connect"
interval_seconds = 5
`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"check-config", "-config", valid}, &stdout, &stderr); code != 0 {
		t.Fatalf("valid: exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ok") {
		t.Errorf("stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"check-config", "-config", unknown}, &stdout, &stderr); code != 1 {
		t.Errorf("unknown keys: exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown keys") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// install.sh が既存の設定ファイルの値を読むのに使う。コメント・リテラル文字列など TOML として正しい書き方を読めること。
func TestRunCheckConfigGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `server_id = 's-1' # literal string
agent_token = "sat_abc"
url    =   "wss://example.com/agent/connect"   # trailing comment
interval = "30s" # custom cadence
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"server_id":   "s-1",
		"agent_token": "sat_abc",
		"url":         "wss://example.com/agent/connect",
		"interval":    "30s",
	} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"check-config", "-config", path, "-get", key}, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit code = %d, stderr = %s", key, code, stderr.String())
		}
		if got := strings.TrimSpace(stdout.String()); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"check-config", "-config", path, "-get", "nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("unknown key: exit code = %d, want 2", code)
	}
}

func TestRunSpec(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"spec"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	var got struct {
		CPU struct {
			Threads int `json:"threads"`
		} `json:"cpu"`
		MemoryTotalBytes uint64 `json:"memoryTotalBytes"`
		Disks            []any  `json:"disks"`
		GPUs             []any  `json:"gpus"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if got.CPU.Threads <= 0 || got.MemoryTotalBytes == 0 || got.Disks == nil || got.GPUs == nil {
		t.Errorf("unexpected output: %s", stdout.String())
	}
}
