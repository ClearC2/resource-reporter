package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExamplePayloadParses(t *testing.T) {
	raw, err := os.ReadFile("example-alertmanager-payload.json")
	if err != nil {
		t.Fatal(err)
	}
	var p WebhookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Alerts) != 1 || p.Alerts[0].Labels.Alertname != "Disk Space" ||
		p.Alerts[0].Labels.Instance != "localhost:9100" || p.Alerts[0].Status != "firing" {
		t.Fatalf("unexpected parse: %+v", p)
	}
}

func writeSettings(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeSettings(t, path, `{"slack":"http://slack.invalid/x","token":"test-token","alerts":{"Disk Space":[{"title":"df","command":"df -h"}]}}`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.RepeatEvery() != 6*time.Hour {
		t.Errorf("RepeatEvery = %v", c.RepeatEvery())
	}
	if c.CommandTimeoutSeconds != 20 || c.ReportBudgetSeconds != 75 || c.HostTimeoutSeconds != 90 ||
		c.SlackTimeoutSeconds != 10 || c.MaxOutputChars != 2900 {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if !c.NoticeUnreachable() {
		t.Error("NoticeUnreachable should default to true")
	}
	if len(c.Alerts["Disk Space"]) != 1 || c.Alerts["Disk Space"][0].Timeout != 0 {
		t.Errorf("alerts not parsed: %+v", c.Alerts)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeSettings(t, path, `{"token":"test-token","repeat_hours":0,"unreachable_notice":false,"max_output_chars":100,
		"alerts":{"A":[{"title":"t","command":"c","timeout":45}]}}`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.RepeatEvery() != 0 {
		t.Errorf("RepeatEvery = %v, want 0", c.RepeatEvery())
	}
	if c.NoticeUnreachable() {
		t.Error("NoticeUnreachable should be false")
	}
	if c.MaxOutputChars != 100 {
		t.Errorf("MaxOutputChars = %d", c.MaxOutputChars)
	}
	if c.Alerts["A"][0].Timeout != 45 {
		t.Errorf("Timeout = %d", c.Alerts["A"][0].Timeout)
	}
}

func TestLoadConfigRejectsEmptyToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeSettings(t, path, `{"slack":"http://slack.invalid/x","token":""}`)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for empty token")
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestConfigStoreKeepsLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeSettings(t, path, `{"token":"test-token"}`)
	s, err := NewConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Get().Token; got != "test-token" {
		t.Fatalf("token = %q", got)
	}
	writeSettings(t, path, `{not json`)
	if got := s.Get().Token; got != "test-token" {
		t.Fatalf("after bad file token = %q, want last good", got)
	}
	writeSettings(t, path, `{"token":"test-token-2"}`)
	if got := s.Get().Token; got != "test-token-2" {
		t.Fatalf("after good file token = %q", got)
	}
}

func TestNewConfigStoreFailsOnBadFirstLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeSettings(t, path, `{not json`)
	if _, err := NewConfigStore(path); err == nil {
		t.Fatal("expected error")
	}
}
