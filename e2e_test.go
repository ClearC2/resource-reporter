package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// e2eHost starts a host copy: the real routes over a settings file holding the given alerts.
func e2eHost(t *testing.T, alertsJSON, extra string) *httptest.Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host.json")
	writeSettings(t, path, `{"token":"test-token","alerts":`+alertsJSON+extra+`}`)
	store, err := NewConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newMux(store, NewReporter(store)))
	t.Cleanup(srv.Close)
	return srv
}

// e2eMonitor starts a monitor copy whose hosts' reports come from hostURLs (host name -> base URL).
// It returns the monitor's server and its Reporter.
func e2eMonitor(t *testing.T, slackURL string, hostURLs map[string]string) (*httptest.Server, *Reporter) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monitor.json")
	writeSettings(t, path, `{"token":"test-token","slack":"`+slackURL+`/hook"}`)
	store, err := NewConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r := NewReporter(store)
	r.ReportURL = func(host string) string { return hostURLs[host] + "/report" }
	r.Poster = &SlackPoster{Sleep: func(time.Duration) {}}
	srv := httptest.NewServer(newMux(store, r))
	t.Cleanup(srv.Close)
	return srv, r
}

func e2eNotify(t *testing.T, monitor *httptest.Server, body string) {
	t.Helper()
	resp, err := http.Post(monitor.URL+"/webhook?token=test-token", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook answered %d", resp.StatusCode)
	}
}

func e2eAlert(name, instance, description string) Alert {
	a := testAlert(instance)
	a.Labels.Alertname = name
	a.Annotations.Description = description
	return a
}

func messageText(m SlackBlocks) string {
	var parts []string
	for _, b := range m.Blocks {
		parts = append(parts, b.Text.Text)
	}
	return strings.Join(parts, "\n")
}

func TestE2ETwoAlertsTwoHosts(t *testing.T) {
	captureLog(t)
	alertsFor := func(out string) string {
		return `{"Disk Space":[{"title":"Out","command":"echo ` + out + `"}],"RAM Used":[{"title":"Out","command":"echo ` + out + `"}]}`
	}
	hostA := e2eHost(t, alertsFor("OUT-ALPHA"), "")
	hostB := e2eHost(t, alertsFor("OUT-BETA"), "")
	slack := newFakeSlack(t, http.StatusOK)
	monitor, rep := e2eMonitor(t, slack.srv.URL, map[string]string{"hostA": hostA.URL, "hostB": hostB.URL})

	e2eNotify(t, monitor, payload(
		e2eAlert("Disk Space", "hostA:9100", "Disk Space on hostA"),
		e2eAlert("RAM Used", "hostB:9100", "RAM Used on hostB")))
	rep.Wait()

	msgs := slack.messages(t)
	if len(msgs) != 2 {
		t.Fatalf("Slack got %d messages, want 2", len(msgs))
	}
	for _, m := range msgs {
		text := messageText(m)
		switch {
		case strings.Contains(m.Blocks[0].Text.Text, "hostA"):
			if !strings.Contains(text, "OUT-ALPHA") || strings.Contains(text, "OUT-BETA") {
				t.Errorf("hostA message = %q", text)
			}
		case strings.Contains(m.Blocks[0].Text.Text, "hostB"):
			if !strings.Contains(text, "OUT-BETA") || strings.Contains(text, "OUT-ALPHA") {
				t.Errorf("hostB message = %q", text)
			}
		default:
			t.Errorf("message header names neither host: %q", m.Blocks[0].Text.Text)
		}
	}
}

func TestE2ERepeatInsideSixHours(t *testing.T) {
	captureLog(t)
	host := e2eHost(t, `{"Disk Space":[{"title":"Out","command":"echo hello"}]}`, "")
	slack := newFakeSlack(t, http.StatusOK)
	monitor, rep := e2eMonitor(t, slack.srv.URL, map[string]string{"web1": host.URL})
	now := testNow
	rep.Now = func() time.Time { return now }
	body := payload(e2eAlert("Disk Space", "web1:9100", "Disk almost full"))

	send := func(want int) {
		t.Helper()
		e2eNotify(t, monitor, body)
		rep.Wait()
		if n := len(slack.raw()); n != want {
			t.Fatalf("at %v the Slack message count is %d, want %d", now.Sub(testNow), n, want)
		}
	}
	send(1)
	send(1) // same instant
	now = testNow.Add(time.Hour)
	send(1)
	now = testNow.Add(6 * time.Hour)
	send(2)
}

func TestE2EOversizedOutput(t *testing.T) {
	captureLog(t)
	host := e2eHost(t, `{"Disk Space":[{"title":"Big","command":"head -c 20000 /dev/zero | tr '\\0' x"}]}`, `,"max_output_chars":30000`)

	var mu sync.Mutex
	var accepted, rejected []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m SlackBlocks
		bad := json.Unmarshal(raw, &m) != nil
		for _, b := range m.Blocks {
			limit := 3000
			if b.Type == "header" {
				limit = 150
			}
			if utf8.RuneCountInString(b.Text.Text) > limit {
				bad = true
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if bad {
			rejected = append(rejected, string(raw))
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "invalid_blocks")
			return
		}
		accepted = append(accepted, string(raw))
	}))
	t.Cleanup(slackSrv.Close)

	monitor, rep := e2eMonitor(t, slackSrv.URL, map[string]string{"web1": host.URL})
	e2eNotify(t, monitor, payload(e2eAlert("Disk Space", "web1:9100", "Disk almost full")))
	rep.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(rejected) != 0 {
		t.Fatalf("Slack rejected %d messages", len(rejected))
	}
	if len(accepted) != 1 {
		t.Fatalf("Slack accepted %d messages, want 1", len(accepted))
	}
	if !strings.Contains(accepted[0], "truncated") {
		t.Errorf("message does not say the output was truncated")
	}
}

// The 2022 host copy replied with exactly this: title, command and output, no other keys.
func TestE2EOldHostCopy(t *testing.T) {
	captureLog(t)
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"commands":[{"title":"Hostname","command":"hostname","output":"oldhost"}]}`)
	}))
	t.Cleanup(old.Close)
	slack := newFakeSlack(t, http.StatusOK)
	monitor, rep := e2eMonitor(t, slack.srv.URL, map[string]string{"oldhost": old.URL})

	e2eNotify(t, monitor, payload(e2eAlert("Disk Space", "oldhost:9100", "Disk almost full")))
	rep.Wait()

	msgs := slack.messages(t)
	if len(msgs) != 1 {
		t.Fatalf("Slack got %d messages, want 1", len(msgs))
	}
	text := messageText(msgs[0])
	if !strings.Contains(text, "Hostname") || !strings.Contains(text, "oldhost") || strings.Contains(text, "No report could be collected") {
		t.Errorf("message = %q, want the old host's report", text)
	}
}

// The 2022 monitor fetched /report with a plain GET and decoded title, command and output.
func TestE2EOldMonitorCopy(t *testing.T) {
	captureLog(t)
	host := e2eHost(t, `{"Disk Space":[{"title":"Hostname","command":"echo newhost"},{"title":"Two","command":"echo second"}]}`, "")

	resp, err := http.Get(host.URL + "/report?token=test-token&alertName=Disk%20Space")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var old struct {
		Commands []struct {
			Title   string `json:"title"`
			Command string `json:"command"`
			Output  string `json:"output"`
		} `json:"commands"`
	}
	dec := json.NewDecoder(resp.Body)
	dec.DisallowUnknownFields() // the three keys are all the old monitor knows about
	if err := dec.Decode(&old); err != nil {
		t.Fatal(err)
	}
	if len(old.Commands) != 2 {
		t.Fatalf("got %d commands, want 2", len(old.Commands))
	}
	want := [][3]string{{"Hostname", "echo newhost", "newhost"}, {"Two", "echo second", "second"}}
	for i, c := range old.Commands {
		if c.Title != want[i][0] || c.Command != want[i][1] || strings.TrimSpace(c.Output) != want[i][2] {
			t.Errorf("command %d = %+v, want %v", i, c, want[i])
		}
	}
}

func TestE2EExamplePayload(t *testing.T) {
	captureLog(t)
	host := e2eHost(t, `{"Disk Space":[{"title":"Out","command":"echo example-output"}]}`, "")
	slack := newFakeSlack(t, http.StatusOK)
	monitor, rep := e2eMonitor(t, slack.srv.URL, map[string]string{"localhost": host.URL})

	body, err := os.ReadFile("example-alertmanager-payload.json")
	if err != nil {
		t.Fatal(err)
	}
	e2eNotify(t, monitor, string(bytes.TrimSpace(body)))
	rep.Wait()

	msgs := slack.messages(t)
	if len(msgs) != 1 {
		t.Fatalf("Slack got %d messages, want 1", len(msgs))
	}
	if !strings.Contains(messageText(msgs[0]), "example-output") {
		t.Errorf("message = %q, want localhost's output", messageText(msgs[0]))
	}
}
