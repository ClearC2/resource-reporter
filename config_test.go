package main

import (
	"encoding/json"
	"os"
	"testing"
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
