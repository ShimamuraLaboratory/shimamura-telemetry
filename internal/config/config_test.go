package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantErr      bool
		wantInterval time.Duration
	}{
		{
			name: "full",
			body: `
server_id = "calc-01"
agent_token = "secret"
url = "wss://telemetry.example.com/agent"
interval = "10s"
`,
			wantInterval: 10 * time.Second,
		},
		{
			name: "default interval",
			body: `
server_id = "calc-01"
agent_token = "secret"
url = "wss://telemetry.example.com/agent"
`,
			wantInterval: DefaultInterval,
		},
		{
			name:    "missing required",
			body:    `url = "wss://telemetry.example.com/agent"`,
			wantErr: true,
		},
		{
			name: "invalid scheme",
			body: `
server_id = "calc-01"
agent_token = "secret"
url = "https://telemetry.example.com/agent"
`,
			wantErr: true,
		},
		{
			name: "interval too short",
			body: `
server_id = "calc-01"
agent_token = "secret"
url = "wss://telemetry.example.com/agent"
interval = "100ms"
`,
			wantErr: true,
		},
		{
			name: "invalid interval",
			body: `
server_id = "calc-01"
agent_token = "secret"
url = "wss://telemetry.example.com/agent"
interval = "five"
`,
			wantErr: true,
		},
		{
			name: "unknown key",
			body: `
server_id = "calc-01"
agent_token = "secret"
url = "wss://telemetry.example.com/agent"
intervel = "5s"
`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.body, 0o600))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got config %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Interval.Duration != tt.wantInterval {
				t.Errorf("interval = %s, want %s", cfg.Interval.Duration, tt.wantInterval)
			}
		})
	}
}

func TestCheckPermissions(t *testing.T) {
	if err := CheckPermissions(writeConfig(t, "", 0o600)); err != nil {
		t.Errorf("0600 should be accepted: %v", err)
	}
	path := writeConfig(t, "", 0o600)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckPermissions(path); err == nil {
		t.Error("0644 should be rejected")
	}
}
