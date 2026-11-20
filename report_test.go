package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// noProcessMatching fails the test if a process whose command line contains pattern is still
// running shortly after the call. pgrep is run directly (not through bash -c) so it cannot
// match itself.
func noProcessMatching(t *testing.T, pattern string) {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Fatalf("pgrep not available: %v", err)
	}
	var out []byte
	for i := 0; i < 20; i++ { // the SIGKILLed process may need a moment to disappear
		var err error
		out, err = exec.Command("pgrep", "-f", pattern).Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return // exit status 1: no process matched
			}
			t.Fatalf("pgrep: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %q still running: pids %s", pattern, strings.TrimSpace(string(out)))
}

func TestRunCommandOutput(t *testing.T) {
	got := RunCommand(context.Background(), "printf 'a\nb\n'", 5*time.Second, 2900)
	if got != "a\nb" {
		t.Fatalf("got %q, want %q", got, "a\nb")
	}
}

func TestRunCommandSetsCLocale(t *testing.T) {
	got := RunCommand(context.Background(), "echo $LC_ALL", 5*time.Second, 2900)
	if got != "C" {
		t.Fatalf("LC_ALL = %q, want C", got)
	}
}

func TestRunCommandKeepsStdoutOnFailure(t *testing.T) {
	got := RunCommand(context.Background(), "echo partial; echo boom >&2; exit 3", 5*time.Second, 2900)
	for _, want := range []string{"partial", "(exit status 3)", "boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q does not contain %q", got, want)
		}
	}
	if !strings.HasPrefix(got, "partial") {
		t.Errorf("stdout should come first: %q", got)
	}
}

func TestRunCommandTimeoutKillsGroup(t *testing.T) {
	start := time.Now()
	got := RunCommand(context.Background(), "echo started; sleep 600 & wait", 300*time.Millisecond, 2900)
	if d := time.Since(start); d >= 3*time.Second {
		t.Errorf("took %v, want < 3s", d)
	}
	if !strings.Contains(got, "started") {
		t.Errorf("output printed before the timeout was lost: %q", got)
	}
	if !strings.Contains(got, "(timed out after") {
		t.Errorf("no timeout note: %q", got)
	}
	if !strings.HasSuffix(got, "(timed out after 300ms)") {
		t.Errorf("output should end with the timeout note: %q", got)
	}
	noProcessMatching(t, "sleep 600")
}

func TestRunCommandBackgroundChildDoesNotHang(t *testing.T) {
	// bash exits at once but leaves a child holding stdout open.
	start := time.Now()
	got := RunCommand(context.Background(), "echo hi; sleep 601 &", 10*time.Second, 2900)
	if d := time.Since(start); d >= 5*time.Second {
		t.Errorf("took %v, want < 5s", d)
	}
	if got != "hi" {
		t.Errorf("got %q, want %q", got, "hi")
	}
	noProcessMatching(t, "sleep 601")
}

func TestRunCommandTruncates(t *testing.T) {
	got := RunCommand(context.Background(), "head -c 10000 /dev/zero | tr '\\0' x", 5*time.Second, 2900)
	if n := utf8.RuneCountInString(got); n > 2900 {
		t.Errorf("len = %d chars, want <= 2900", n)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("no truncation note: %q", got[len(got)-80:])
	}
}

func TestRunCommandDrainsLargeOutput(t *testing.T) {
	// 1 MB of stdout is far beyond the 64 KiB kept; the rest must be drained, not left blocking.
	got := RunCommand(context.Background(), "head -c 1000000 /dev/zero | tr '\\0' x; echo done", 10*time.Second, 2900)
	if strings.Contains(got, "timed out") {
		t.Fatalf("command blocked on a full pipe: %q", got[len(got)-80:])
	}
	if n := utf8.RuneCountInString(got); n > 2900 || !strings.Contains(got, "truncated") {
		t.Errorf("len = %d chars, tail %q", n, got[len(got)-80:])
	}
}

func testConfig(alerts map[string][]*CommandSection, budgetSeconds int) *Config {
	c := &Config{Token: "test-token", Alerts: alerts, ReportBudgetSeconds: budgetSeconds}
	c.applyDefaults()
	return c
}

func TestRunReportDoesNotMutateConfig(t *testing.T) {
	cfg := testConfig(map[string][]*CommandSection{
		"X": {{Title: "one", Command: "echo one"}, {Title: "two", Command: "echo two", Timeout: 45}},
	}, 0)
	got := RunReport(context.Background(), cfg, "X")
	if cfg.Alerts["X"][0].Output != "" || cfg.Alerts["X"][1].Output != "" {
		t.Fatalf("RunReport wrote into cfg: %+v %+v", cfg.Alerts["X"][0], cfg.Alerts["X"][1])
	}
	if len(got) != 2 || got[0].Title != "one" || got[0].Output != "one" || got[1].Output != "two" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestRunReportUnknownAlert(t *testing.T) {
	got := RunReport(context.Background(), testConfig(nil, 0), "nope")
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want empty non-nil slice", got)
	}
}

func TestRunReportBudget(t *testing.T) {
	cfg := testConfig(map[string][]*CommandSection{
		"X": {
			{Title: "a", Command: "sleep 1.2"},
			{Title: "b", Command: "sleep 0.6"},
			{Title: "c", Command: "sleep 0.6"},
		},
	}, 2)
	start := time.Now()
	got := RunReport(context.Background(), cfg, "X")
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("report took %v, budget is 2s", d)
	}
	if len(got) != 3 {
		t.Fatalf("got %d sections", len(got))
	}
	if got[0].Output != "" {
		t.Errorf("first command should finish: %q", got[0].Output)
	}
	// About 0.8s is left for the second command: less than a second is not worth starting.
	for i := 1; i < 3; i++ {
		if got[i].Output != "(skipped: report time budget used up)" {
			t.Errorf("output %d = %q, want the budget skip note", i, got[i].Output)
		}
	}
}

func TestFormatTimeout(t *testing.T) {
	for d, want := range map[time.Duration]string{
		20 * time.Second:                           "20s",
		14998231407 * time.Nanosecond:              "15s",
		time.Second + 200*time.Millisecond:         "1s",
		300 * time.Millisecond:                     "300ms",
		time.Duration(1700) * time.Millisecond:     "2s",
		time.Duration(999999999) * time.Nanosecond: "999.999999ms",
	} {
		if got := formatTimeout(d); got != want {
			t.Errorf("formatTimeout(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestRunReportBudgetClipPrintsWholeSeconds(t *testing.T) {
	// Budget 2s: the second command is clipped to the ~1.7s left, an arbitrary duration.
	cfg := testConfig(map[string][]*CommandSection{
		"X": {{Title: "a", Command: "sleep 0.3"}, {Title: "b", Command: "sleep 10"}},
	}, 2)
	got := RunReport(context.Background(), cfg, "X")
	if len(got) != 2 {
		t.Fatalf("got %d sections", len(got))
	}
	if !regexp.MustCompile(`\(timed out after [0-9]+s\)$`).MatchString(got[1].Output) {
		t.Errorf("clipped timeout should print whole seconds: %q", got[1].Output)
	}
}

func TestReportHandler(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeSettings(t, path, `{"token":"test-token","alerts":{"Disk Space":[
		{"title":"one","command":"echo one"},
		{"title":"two","command":"echo two","timeout":45}]}}`)
	store, err := NewConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	h := reportHandler(store)
	log.SetOutput(io.Discard) // the token rejections below are expected
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	tests := []struct {
		name       string
		method     string
		query      string
		wantStatus int
		wantBody   string // exact body when set
		contains   string // substring when set
	}{
		{name: "known alert", method: "GET", query: "token=test-token&alertName=Disk+Space", wantStatus: 200},
		{name: "unknown alert", method: "GET", query: "token=test-token&alertName=Nope", wantStatus: 200, contains: `"commands":[]`},
		{name: "wrong token", method: "GET", query: "token=wrong&alertName=Disk+Space", wantStatus: 400, wantBody: `{"error":"Invalid token"}`},
		{name: "no token", method: "GET", query: "alertName=Disk+Space", wantStatus: 400, wantBody: `{"error":"Invalid token"}`},
		{name: "no alert name", method: "GET", query: "token=test-token", wantStatus: 400, wantBody: `{"error":"Missing alert name"}`},
		{name: "post", method: "POST", query: "token=test-token&alertName=Disk+Space", wantStatus: 405},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(tc.method, "/report?"+tc.query, nil))
			body := rec.Body.String()
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d; body %s", rec.Code, tc.wantStatus, body)
			}
			if tc.wantBody != "" && strings.TrimSpace(body) != tc.wantBody {
				t.Errorf("body %s, want %s", body, tc.wantBody)
			}
			if tc.contains != "" && !strings.Contains(body, tc.contains) {
				t.Errorf("body %s does not contain %s", body, tc.contains)
			}
			if tc.wantStatus != 405 && rec.Header().Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
			}
		})
	}

	t.Run("reply shape", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/report?token=test-token&alertName=Disk+Space", nil))
		var reply struct {
			Commands []map[string]any `json:"commands"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatalf("bad JSON %s: %v", rec.Body.String(), err)
		}
		if len(reply.Commands) != 2 {
			t.Fatalf("got %d commands: %s", len(reply.Commands), rec.Body.String())
		}
		want := []map[string]any{
			{"title": "one", "command": "echo one", "output": "one"},
			{"title": "two", "command": "echo two", "output": "two"},
		}
		for i, c := range reply.Commands {
			if len(c) != 3 {
				t.Errorf("entry %d has keys other than title/command/output: %v", i, c)
			}
			for k, v := range want[i] {
				if c[k] != v {
					t.Errorf("entry %d %s = %v, want %v", i, k, c[k], v)
				}
			}
		}
		if strings.Contains(rec.Body.String(), "timeout") {
			t.Errorf("reply leaks the timeout key: %s", rec.Body.String())
		}
	})
}

func TestReportHandlerOutlivesServerWriteTimeout(t *testing.T) {
	// The server's WriteTimeout (1s) is shorter than the report (~1.5s, budget 5s): the handler's own
	// write deadline must keep the reply from being dropped.
	path := filepath.Join(t.TempDir(), "config.json")
	writeSettings(t, path, `{"token":"test-token","report_budget_seconds":5,
		"alerts":{"Slow":[{"title":"slow","command":"sleep 1.5; echo done"}]}}`)
	store, err := NewConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(reportHandler(store))
	srv.Config.WriteTimeout = time.Second
	srv.Start()
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/report?token=test-token&alertName=Slow")
	if err != nil {
		t.Fatalf("reply did not arrive: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"output":"done"`) {
		t.Fatalf("status %d, body %s, err %v", resp.StatusCode, body, err)
	}
}

func TestValidTokenLogsAddressNotToken(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	cfg := &Config{Token: "test-token"}
	r := httptest.NewRequest(http.MethodGet, "/report?token=supplied-secret&alertName=A", nil)
	r.RemoteAddr = "192.0.2.7:41234"
	if validToken(cfg, r) {
		t.Fatal("wrong token accepted")
	}
	logged := buf.String()
	if !strings.Contains(logged, "192.0.2.7:41234") {
		t.Errorf("log does not name the remote address: %q", logged)
	}
	if strings.Contains(logged, "supplied-secret") || strings.Contains(logged, "test-token") {
		t.Errorf("log contains a token: %q", logged)
	}

	ok := httptest.NewRequest(http.MethodGet, "/report?token=test-token", nil)
	if !validToken(cfg, ok) {
		t.Error("right token rejected")
	}
	if validToken(&Config{Token: ""}, httptest.NewRequest(http.MethodGet, "/report?token=", nil)) {
		t.Error("an empty configured token must never match")
	}
}

func TestRunCommandTinyMaxCharsNeverExceeds(t *testing.T) {
	// The exit note alone is longer than maxChars: the result must still fit.
	got := RunCommand(context.Background(), "echo hello; exit 3", 5*time.Second, 5)
	if n := utf8.RuneCountInString(got); n > 5 {
		t.Errorf("len = %d chars, want <= 5: %q", n, got)
	}
	got = RunCommand(context.Background(), "echo hello; exit 3", 5*time.Second, 40)
	if !strings.Contains(got, "exit status 3") || utf8.RuneCountInString(got) > 40 {
		t.Errorf("normal case lost the exit note: %q", got)
	}
}
