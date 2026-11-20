package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog sends the log package's output to a buffer for the rest of the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	b := &lockedBuffer{}
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return b
}

// fakeSlack records every message posted to it and answers with status.
type fakeSlack struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	status int
}

func newFakeSlack(t *testing.T, status int) *fakeSlack {
	t.Helper()
	f := &fakeSlack{status: status}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		f.mu.Unlock()
		w.WriteHeader(f.status)
		if f.status != http.StatusOK {
			io.WriteString(w, "invalid_payload")
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) raw() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

func (f *fakeSlack) messages(t *testing.T) []SlackBlocks {
	t.Helper()
	var out []SlackBlocks
	for _, b := range f.raw() {
		var m SlackBlocks
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			t.Fatalf("fake Slack got invalid JSON %q: %v", b, err)
		}
		out = append(out, m)
	}
	return out
}

// hostCall is one request the fake host received.
type hostCall struct {
	Host, Token, AlertName string
}

// fakeHost plays any number of hosts; the host name arrives in the "host" query parameter.
type fakeHost struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []hostCall
}

func newFakeHost(t *testing.T, reply http.HandlerFunc) *fakeHost {
	t.Helper()
	f := &fakeHost{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.calls = append(f.calls, hostCall{q.Get("host"), q.Get("token"), q.Get("alertName")})
		f.mu.Unlock()
		reply(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHost) Calls() []hostCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hostCall(nil), f.calls...)
}

func replyJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
}

var testNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// newTestReporter builds a Reporter whose settings post to slackURL and whose reports come from
// baseURL (empty slackURL leaves the slack key out). extra is appended inside the settings object.
func newTestReporter(t *testing.T, slackURL, baseURL, extra string) *Reporter {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	settings := `{"token":"test-token"`
	if slackURL != "" {
		settings += `,"slack":"` + slackURL + `/hook"`
	}
	writeSettings(t, path, settings+extra+"}")
	store, err := NewConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r := NewReporter(store)
	r.ReportURL = func(host string) string { return baseURL + "/report?host=" + url.QueryEscape(host) }
	r.Now = func() time.Time { return testNow }
	r.Poster = &SlackPoster{Sleep: func(time.Duration) {}}
	return r
}

func testAlert(instance string) Alert {
	var a Alert
	a.Status = "firing"
	a.Labels.Alertname = "Disk Space"
	a.Labels.Instance = instance
	a.Annotations.Description = "Disk almost full"
	a.StartsAt = "2026-11-20T11:55:00Z"
	return a
}

// closedURL returns the address of a server that is no longer listening.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func TestAlertHost(t *testing.T) {
	cfg := &Config{Hosts: map[string]string{"friendly": "10.0.0.5"}}
	cases := []struct {
		instance string
		want     string
		ok       bool
	}{
		{"web1.clearc2.com:9100", "web1.clearc2.com", true},
		{"friendly", "10.0.0.5", true},
		{"web2", "web2", true},
		{"[::1]:9100", "[::1]", true},
		{"https://site/path", "", false},
		{"", "", false},
		{"qemu/118", "", false},
		{"two words:9100", "", false},
		{"{bad}:9100", "", false},
		{":9100", "", false},
		{"user@other:9100", "", false},
		{"host?x=1:9100", "", false},
		{"host#frag", "", false},
		{`back\slash:9100`, "", false},
		{"per%cent:9100", "", false},
		{`quo"te:9100`, "", false},
		{"it's:9100", "", false},
		{"new\nline:9100", "", false},
		{"tab\there:9100", "", false},
		{"host\x00:9100", "", false},
		{"hôst:9100", "", false},
		{"Web_1-a.Example.com:9100", "Web_1-a.Example.com", true},
		{"192.168.100.5:9100", "192.168.100.5", true},
		{"[fe80::1]:9100", "[fe80::1]", true},
	}
	for _, c := range cases {
		host, ok := AlertHost(cfg, testAlert(c.instance))
		if ok != c.ok || (ok && host != c.want) {
			t.Errorf("AlertHost(%q) = %q, %v; want %q, %v", c.instance, host, ok, c.want, c.ok)
		}
	}
}

// claimed reports whether ShouldReport gave the caller the alert (fresh or retry).
func claimed(s *SeenAlerts, key string, now time.Time, every time.Duration) bool {
	ok, _ := s.ShouldReport(key, now, every)
	return ok
}

func TestSeenAlerts(t *testing.T) {
	s := NewSeenAlerts()
	every := 6 * time.Hour
	if ok, retry := s.ShouldReport("k", testNow, every); !ok || retry {
		t.Fatalf("first report should go out as a fresh claim: ok=%v retry=%v", ok, retry)
	}
	s.MarkReported("k", testNow)
	if claimed(s, "k", testNow.Add(5*time.Hour), every) {
		t.Fatal("repeat after 5h should be quiet")
	}
	if !claimed(s, "k", testNow.Add(6*time.Hour), every) {
		t.Fatal("repeat after 6h should go out")
	}
	for i := 0; i < 3; i++ {
		if !claimed(s, "z", testNow, 0) {
			t.Fatal("every == 0 should always report")
		}
	}
	s.ShouldReport("old", testNow, every)
	s.ShouldReport("other", testNow.Add(12*time.Hour+time.Second), every)
	s.mu.Lock()
	_, kept := s.last["old"]
	s.mu.Unlock()
	if kept {
		t.Error("entry older than 2*every was not dropped")
	}
	s.Forget("other")
	if !claimed(s, "other", testNow.Add(12*time.Hour+2*time.Second), every) {
		t.Error("forgotten key should report again")
	}
}

func TestSeenAlertsNoticeOnly(t *testing.T) {
	s := NewSeenAlerts()
	every := 6 * time.Hour
	s.ShouldReport("k", testNow, every)
	s.MarkNotice("k")
	ok, retry := s.ShouldReport("k", testNow.Add(time.Hour), every)
	if !ok || !retry {
		t.Fatalf("after a notice the host should be asked again as a retry: ok=%v retry=%v", ok, retry)
	}
	if claimed(s, "k", testNow.Add(time.Hour), every) {
		t.Fatal("a retry in flight must not be claimed twice")
	}
	s.Release("k")
	if ok, retry := s.ShouldReport("k", testNow.Add(2*time.Hour), every); !ok || !retry {
		t.Fatalf("released retry should be claimable as a retry again: ok=%v retry=%v", ok, retry)
	}
	s.MarkReported("k", testNow.Add(2*time.Hour))
	if claimed(s, "k", testNow.Add(7*time.Hour), every) {
		t.Fatal("a report after a notice starts the normal window from the report")
	}
	if ok, retry := s.ShouldReport("k", testNow.Add(8*time.Hour), every); !ok || retry {
		t.Fatalf("after the window a fresh claim is due: ok=%v retry=%v", ok, retry)
	}
}

func TestSeenAlertsConcurrentClaim(t *testing.T) {
	race := func(s *SeenAlerts) int {
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if claimed(s, "k", testNow.Add(time.Minute), time.Hour) {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return wins
	}
	if wins := race(NewSeenAlerts()); wins != 1 {
		t.Errorf("%d goroutines claimed the same new alert, want 1", wins)
	}
	s := NewSeenAlerts()
	s.ShouldReport("k", testNow, time.Hour)
	s.MarkNotice("k")
	if wins := race(s); wins != 1 {
		t.Errorf("%d goroutines claimed the same notice-only alert, want 1", wins)
	}
}

func TestProcessAlertPostsReport(t *testing.T) {
	captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	host := newFakeHost(t, replyJSON(`{"commands":[
		{"title":"df","command":"df -h","output":"/ 91%"},
		{"title":"du","command":"du -sh /var","output":"40G /var"}]}`))
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
	r.ProcessAlert(context.Background(), testAlert("web1.clearc2.com:9100"))

	msgs := slack.messages(t)
	if len(msgs) != 1 {
		t.Fatalf("Slack got %d messages, want 1", len(msgs))
	}
	if n := len(msgs[0].Blocks); n != 5 {
		t.Fatalf("message has %d blocks, want 5: %+v", n, msgs[0])
	}
	if h := msgs[0].Blocks[0]; h.Type != "header" || h.Text.Text != "Disk almost full" {
		t.Errorf("header = %+v", h)
	}
	if !strings.Contains(msgs[0].Blocks[2].Text.Text, "/ 91%") {
		t.Errorf("first output block = %q", msgs[0].Blocks[2].Text.Text)
	}
	calls := host.Calls()
	if len(calls) != 1 || calls[0] != (hostCall{"web1.clearc2.com", "test-token", "Disk Space"}) {
		t.Errorf("host calls = %+v", calls)
	}
}

func TestProcessAlertTitleOnly(t *testing.T) {
	captureLog(t)
	for _, body := range []string{`{"commands":[]}`, `{"commands":null}`, `{}`} {
		slack := newFakeSlack(t, http.StatusOK)
		host := newFakeHost(t, replyJSON(body))
		r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
		r.ProcessAlert(context.Background(), testAlert("web1:9100"))
		msgs := slack.messages(t)
		if len(msgs) != 1 || len(msgs[0].Blocks) != 1 {
			t.Errorf("reply %s: messages = %+v, want 1 message with 1 block", body, msgs)
		}
	}
}

func TestProcessAlertUnreachable(t *testing.T) {
	captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	r := newTestReporter(t, slack.srv.URL, closedURL(t), "")
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))

	msgs := slack.messages(t)
	if len(msgs) != 1 || len(msgs[0].Blocks) != 2 {
		t.Fatalf("messages = %+v, want 1 message with 2 blocks", msgs)
	}
	note := msgs[0].Blocks[1].Text.Text
	if want := "No report could be collected from web1: connection refused"; note != want {
		t.Errorf("note = %q, want %q", note, want)
	}
	if msgs[0].Blocks[0].Text.Text != "Disk almost full" {
		t.Errorf("header = %q", msgs[0].Blocks[0].Text.Text)
	}
}

func TestProcessAlertUnreachableNoticeOff(t *testing.T) {
	logs := captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	r := newTestReporter(t, slack.srv.URL, closedURL(t), `,"unreachable_notice":false`)
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	if n := len(slack.raw()); n != 0 {
		t.Errorf("Slack got %d messages, want 0", n)
	}
	if !strings.Contains(logs.String(), "web1") {
		t.Errorf("no log line names the host: %q", logs.String())
	}
	// Nothing was posted, so the same alert must not wait out repeat_hours: it tries the host again.
	var hostCalls int
	r.ReportURL = func(host string) string { hostCalls++; return closedURL(t) + "/report" }
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	if hostCalls != 1 {
		t.Errorf("second notification made %d host calls, want 1", hostCalls)
	}
	if n := len(slack.raw()); n != 0 {
		t.Errorf("Slack got %d messages after the second try, want 0", n)
	}
}

// switchHost answers 500 while down is true, else with a one-command report.
func switchHost(t *testing.T, down *atomic.Bool) *fakeHost {
	return newFakeHost(t, func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		replyJSON(`{"commands":[{"title":"df","command":"df -h","output":"/ 91%"}]}`)(w, r)
	})
}

func TestProcessAlertNoticeThenRecovery(t *testing.T) {
	captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	var down atomic.Bool
	down.Store(true)
	host := switchHost(t, &down)
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
	now := testNow
	r.Now = func() time.Time { return now }

	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	down.Store(false)
	now = testNow.Add(time.Hour)
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	now = testNow.Add(2 * time.Hour)
	r.ProcessAlert(context.Background(), testAlert("web1:9100")) // reported now: quiet

	msgs := slack.messages(t)
	if len(msgs) != 2 {
		t.Fatalf("Slack got %d messages, want 1 notice + 1 report", len(msgs))
	}
	if !strings.Contains(messageText(msgs[0]), "No report could be collected from web1: host replied 500") {
		t.Errorf("first message = %q, want the notice", messageText(msgs[0]))
	}
	if !strings.Contains(messageText(msgs[1]), "/ 91%") {
		t.Errorf("second message = %q, want the report", messageText(msgs[1]))
	}
	if n := len(host.Calls()); n != 2 {
		t.Errorf("host got %d calls, want 2", n)
	}
}

func TestProcessAlertNoticeOnceWhileUnreachable(t *testing.T) {
	captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	var down atomic.Bool
	down.Store(true)
	host := switchHost(t, &down)
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
	now := testNow
	r.Now = func() time.Time { return now }

	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	now = testNow.Add(time.Hour)
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	if n := len(slack.raw()); n != 1 {
		t.Errorf("Slack got %d messages inside the window, want exactly 1 notice", n)
	}
	if n := len(host.Calls()); n != 2 {
		t.Errorf("host got %d calls, want 2", n)
	}
	// After repeat_hours the cycle starts over: a new notice may be posted.
	now = testNow.Add(6 * time.Hour)
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	if n := len(slack.raw()); n != 2 {
		t.Errorf("Slack got %d messages after the window, want 2", n)
	}
}

func TestProcessAlertConcurrentRetryPostsOnce(t *testing.T) {
	captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	var down atomic.Bool
	down.Store(true)
	host := switchHost(t, &down)
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
	r.ProcessAlert(context.Background(), testAlert("web1:9100")) // notice
	down.Store(false)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.ProcessAlert(context.Background(), testAlert("web1:9100"))
		}()
	}
	wg.Wait()
	if n := len(slack.raw()); n != 2 {
		t.Errorf("Slack got %d messages, want 1 notice + 1 report", n)
	}
}

func TestProcessAlertHostTimeout(t *testing.T) {
	captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	host := newFakeHost(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	})
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, `,"host_timeout_seconds":1`)
	start := time.Now()
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("ProcessAlert took %v with a 1s host timeout", d)
	}
	msgs := slack.messages(t)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Blocks[len(msgs[0].Blocks)-1].Text.Text, "web1: timed out") {
		t.Errorf("messages = %+v, want a timed out notice", msgs)
	}
}

func TestProcessAlertHostRefuses(t *testing.T) {
	captureLog(t)
	cases := []struct {
		name   string
		reply  http.HandlerFunc
		reason string
	}{
		{"token mismatch", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"Invalid token"}`)
		}, "host replied 400"},
		{"html", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<html><body>It works</body></html>")
		}, "unreadable reply"},
		{"cut-off json", replyJSON(`{"commands":[{"title":"df","outp`), "unreadable reply"},
		{"json null", replyJSON(`null`), "unreadable reply"},
		{"json array", replyJSON(`[]`), "unreadable reply"},
		{"over 1 MiB", replyJSON(`{"commands":[{"title":"t","command":"c","output":"` +
			strings.Repeat("x", maxHostReply) + `"}]}`), "reply too large"},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}, "host replied 302"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slack := newFakeSlack(t, http.StatusOK)
			host := newFakeHost(t, c.reply)
			r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
			r.ProcessAlert(context.Background(), testAlert("web1:9100"))
			msgs := slack.messages(t)
			if len(msgs) != 1 {
				t.Fatalf("Slack got %d messages, want 1", len(msgs))
			}
			want := "No report could be collected from web1: " + c.reason
			if got := msgs[0].Blocks[len(msgs[0].Blocks)-1].Text.Text; got != want {
				t.Errorf("note = %q, want %q", got, want)
			}
			if n := len(host.Calls()); n != 1 {
				t.Errorf("host got %d calls, want 1 (redirects are not followed)", n)
			}
		})
	}
}

func TestProcessAlertSkipsNonHost(t *testing.T) {
	logs := captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	host := newFakeHost(t, replyJSON(`{"commands":[]}`))
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
	for _, instance := range []string{"https://site/path", ""} {
		r.ProcessAlert(context.Background(), testAlert(instance))
	}
	if n := len(host.Calls()); n != 0 {
		t.Errorf("host got %d calls, want 0", n)
	}
	if n := len(slack.raw()); n != 0 {
		t.Errorf("Slack got %d messages, want 0", n)
	}
	if n := strings.Count(logs.String(), "\n"); n != 2 {
		t.Errorf("want one log line per skipped alert, got %d: %q", n, logs.String())
	}
}

func TestProcessAlertReportHosts(t *testing.T) {
	logs := captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	host := newFakeHost(t, replyJSON(`{"commands":[]}`))
	r := newTestReporter(t, slack.srv.URL, host.srv.URL,
		`,"report_hosts":["web1.clearc2.com",".prod.clearc2.com","192.168.100.0/24"],"hosts":{"friendly":"10.9.9.9"}`)
	for _, instance := range []string{"WEB1.clearc2.com:9100", "db1.prod.clearc2.com:9100", "192.168.100.7:9100", "friendly"} {
		r.ProcessAlert(context.Background(), testAlert(instance))
	}
	if n := len(host.Calls()); n != 4 {
		t.Fatalf("allowed hosts: host got %d calls, want 4", n)
	}
	before := logs.String()
	for _, instance := range []string{"site.example.com:443", "prod.clearc2.com:9100", "192.168.101.7:9100"} {
		r.ProcessAlert(context.Background(), testAlert(instance))
	}
	if n := len(host.Calls()); n != 4 {
		t.Errorf("host got %d calls after the skipped alerts, want still 4", n)
	}
	if n := len(slack.raw()); n != 4 {
		t.Errorf("Slack got %d messages, want 4 (none for skipped hosts)", n)
	}
	skipped := strings.TrimPrefix(logs.String(), before)
	if n := strings.Count(skipped, "\n"); n != 3 {
		t.Errorf("want one log line per skipped alert, got %d: %q", n, skipped)
	}
	if !strings.Contains(skipped, `"site.example.com"`) || !strings.Contains(skipped, `"Disk Space"`) {
		t.Errorf("skip line should name the host (quoted) and the alert: %q", skipped)
	}
}

func TestProcessAlertWithoutSlackURL(t *testing.T) {
	logs := captureLog(t)
	host := newFakeHost(t, replyJSON(`{"commands":[]}`))
	r := newTestReporter(t, "", host.srv.URL, "")
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	if n := len(host.Calls()); n != 0 {
		t.Errorf("host got %d calls, want 0", n)
	}
	if n := strings.Count(logs.String(), "\n"); n != 1 {
		t.Errorf("want one log line, got %q", logs.String())
	}
}

func TestProcessAlertSkipsRepeat(t *testing.T) {
	captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	host := newFakeHost(t, replyJSON(`{"commands":[]}`))
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	if n := len(host.Calls()); n != 1 {
		t.Errorf("host got %d calls, want 1", n)
	}
	if n := len(slack.raw()); n != 1 {
		t.Errorf("Slack got %d messages, want 1", n)
	}
}

func TestProcessAlertForgetsOnSlackFailure(t *testing.T) {
	logs := captureLog(t)
	slack := newFakeSlack(t, http.StatusBadRequest)
	host := newFakeHost(t, replyJSON(`{"commands":[]}`))
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	if !strings.Contains(logs.String(), "400") {
		t.Errorf("Slack failure not logged: %q", logs.String())
	}
	r.ProcessAlert(context.Background(), testAlert("web1:9100"))
	if n := len(host.Calls()); n != 2 {
		t.Errorf("host got %d calls, want 2", n)
	}
	if n := len(slack.raw()); n != 2 {
		t.Errorf("Slack got %d post attempts, want 2", n)
	}
}

func TestProcessAlertNeverLeaksToken(t *testing.T) {
	logs := captureLog(t)
	refuses := newFakeHost(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"Invalid token"}`)
	})
	works := newFakeHost(t, replyJSON(`{"commands":[{"title":"t","command":"c","output":"o"}]}`))
	okSlack := newFakeSlack(t, http.StatusOK)
	badSlack := newFakeSlack(t, http.StatusInternalServerError)

	newTestReporter(t, okSlack.srv.URL, closedURL(t), "").ProcessAlert(context.Background(), testAlert("web1:9100"))
	newTestReporter(t, okSlack.srv.URL, refuses.srv.URL, "").ProcessAlert(context.Background(), testAlert("web2:9100"))
	newTestReporter(t, badSlack.srv.URL, works.srv.URL, "").ProcessAlert(context.Background(), testAlert("web3:9100"))
	newTestReporter(t, badSlack.srv.URL, closedURL(t), "").ProcessAlert(context.Background(), testAlert("web4:9100"))

	out := logs.String()
	if out == "" {
		t.Fatal("no log output captured")
	}
	if strings.Contains(out, "test-token") {
		t.Errorf("token in log output:\n%s", out)
	}
	bodies := append(okSlack.raw(), badSlack.raw()...)
	if len(bodies) < 4 {
		t.Fatalf("Slack got %d bodies, want at least 4", len(bodies))
	}
	for _, b := range bodies {
		if strings.Contains(b, "test-token") {
			t.Errorf("token in Slack message: %s", b)
		}
	}
}

func postWebhook(t *testing.T, r *Reporter, method, query, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/webhook"+query, strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.webhookHandler()(rec, req)
	return rec
}

func payload(alerts ...Alert) string {
	b, _ := json.Marshal(WebhookPayload{Status: "firing", Alerts: alerts})
	return string(b)
}

func TestWebhookHandlerAnswersBeforeHostsDo(t *testing.T) {
	captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	host := newFakeHost(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		replyJSON(`{"commands":[]}`)(w, r)
	})
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
	start := time.Now()
	rec := postWebhook(t, r, http.MethodPost, "?token=test-token", payload(testAlert("web1:9100")))
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("handler took %v", d)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d", rec.Code)
	}
	r.Wait()
	if n := len(slack.raw()); n != 1 {
		t.Errorf("Slack got %d messages after Wait, want 1", n)
	}
}

func TestWebhookHandlerOneReportPerAlert(t *testing.T) {
	captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	host := newFakeHost(t, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(`{"commands":[{"title":"name","command":"hostname","output":"`+r.URL.Query().Get("host")+`"}]}`)(w, r)
	})
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
	var alerts []Alert
	for _, h := range []string{"web1", "web2", "web3"} {
		a := testAlert(h + ":9100")
		a.Annotations.Description = "Disk almost full on " + h
		alerts = append(alerts, a)
	}
	postWebhook(t, r, http.MethodPost, "?token=test-token", payload(alerts...))
	r.Wait()

	var hosts []string
	for _, c := range host.Calls() {
		hosts = append(hosts, c.Host)
	}
	sort.Strings(hosts)
	if strings.Join(hosts, ",") != "web1,web2,web3" {
		t.Errorf("hosts called = %v", hosts)
	}
	msgs := slack.messages(t)
	if len(msgs) != 3 {
		t.Fatalf("Slack got %d messages, want 3", len(msgs))
	}
	for _, m := range msgs {
		h := strings.TrimPrefix(m.Blocks[0].Text.Text, "Disk almost full on ")
		if !strings.Contains(m.Blocks[2].Text.Text, h) {
			t.Errorf("message for %s carries output %q", h, m.Blocks[2].Text.Text)
		}
	}
}

func TestWebhookHandlerIgnoresResolved(t *testing.T) {
	captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	host := newFakeHost(t, replyJSON(`{"commands":[]}`))
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
	a := testAlert("web1:9100")
	a.Status = "resolved"
	rec := postWebhook(t, r, http.MethodPost, "?token=test-token", payload(a))
	r.Wait()
	if rec.Code != http.StatusOK {
		t.Errorf("status %d", rec.Code)
	}
	if n := len(host.Calls()); n != 0 {
		t.Errorf("host got %d calls, want 0", n)
	}
}

func TestWebhookHandlerRejects(t *testing.T) {
	captureLog(t)
	host := newFakeHost(t, replyJSON(`{"commands":[]}`))
	r := newTestReporter(t, "http://slack.invalid", host.srv.URL, "")

	rec := postWebhook(t, r, http.MethodPost, "?token=wrong", payload(testAlert("web1:9100")))
	if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != `{"error":"Invalid token"}` {
		t.Errorf("bad token: %d %q", rec.Code, rec.Body.String())
	}
	rec = postWebhook(t, r, http.MethodPost, "?token=test-token", "{not json")
	if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != `{"error":"Could not parse payload"}` {
		t.Errorf("bad body: %d %q", rec.Code, rec.Body.String())
	}
	rec = postWebhook(t, r, http.MethodGet, "?token=test-token", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
	r.Wait()
	if n := len(host.Calls()); n != 0 {
		t.Errorf("host got %d calls, want 0", n)
	}
}

func TestWebhookHandlerRecoversPanic(t *testing.T) {
	logs := captureLog(t)
	slack := newFakeSlack(t, http.StatusOK)
	host := newFakeHost(t, replyJSON(`{"commands":[]}`))
	r := newTestReporter(t, slack.srv.URL, host.srv.URL, "")
	r.ReportURL = func(h string) string {
		if h == "web1" {
			panic("boom")
		}
		return host.srv.URL + "/report?host=" + h
	}
	postWebhook(t, r, http.MethodPost, "?token=test-token", payload(testAlert("web1:9100"), testAlert("web2:9100")))
	r.Wait()
	if !strings.Contains(logs.String(), "boom") {
		t.Errorf("panic not logged: %q", logs.String())
	}
	if n := len(slack.raw()); n != 1 {
		t.Errorf("Slack got %d messages, want 1 (the other alert still reported)", n)
	}
}
