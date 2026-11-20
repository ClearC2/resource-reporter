package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestLoadConfigReportHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeSettings(t, path, `{"token":"test-token","report_hosts":["web1.Example.com",".clearc2.com","192.168.100.0/24","fd00::/8","10.1.2.3"]}`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"web1.example.com":     true, // exact, case-insensitive
		"WEB1.EXAMPLE.COM":     true,
		"web2.example.com":     false,
		"db01.clearc2.com":     true, // suffix
		"a.b.ClearC2.com":      true,
		"clearc2.com":          false, // the suffix needs a label in front
		"evilclearc2.com":      false,
		"clearc2.com.evil.net": false,
		"192.168.100.27":       true, // CIDR
		"192.168.101.27":       false,
		"[fd00::5]":            true,
		"[fe80::1]":            false,
		"site.example.com":     false,
		"10.1.2.3":             true, // single address
		"10.1.2.4":             false,
	} {
		if got := c.ReportHostAllowed(host); got != want {
			t.Errorf("ReportHostAllowed(%q) = %v, want %v", host, got, want)
		}
	}

	open := &Config{}
	if !open.ReportHostAllowed("anything.example.org") {
		t.Error("without report_hosts every host is allowed")
	}
	writeSettings(t, path, `{"token":"test-token","report_hosts":[]}`)
	if c, err := LoadConfig(path); err != nil || !c.ReportHostAllowed("anything.example.org") {
		t.Errorf("an empty report_hosts list allows every host: %v", err)
	}
}

func TestLoadConfigRejectsBadReportHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, entry := range []string{`""`, `"."`, `"*.example.com"`, `"192.168.100.0/33"`, `"10.0.0.0/x"`,
		`"bad host"`, `"web1:9100"`, `"http://web1"`, `5`} {
		writeSettings(t, path, `{"token":"test-token","report_hosts":["ok.example.com",`+entry+`]}`)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("report_hosts entry %s was accepted", entry)
		}
	}
}

func TestConfigStoreKeepsLastGood(t *testing.T) {
	logs := captureLog(t) // the bad file below is expected to log "settings: reload failed"
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
	if !strings.Contains(logs.String(), "settings: reload failed") {
		t.Errorf("bad file not logged: %q", logs.String())
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
