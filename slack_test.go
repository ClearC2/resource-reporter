package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTruncateOutput(t *testing.T) {
	if got := TruncateOutput("short", 2900); got != "short" {
		t.Errorf("short text changed: %q", got)
	}
	long := strings.Repeat("x", 10000)
	got := TruncateOutput(long, 2900)
	if n := utf8.RuneCountInString(got); n > 2900 {
		t.Errorf("len = %d chars, want <= 2900", n)
	}
	head := strings.TrimSuffix(got[:strings.Index(got, "\n… truncated (")], "")
	wantNote := "\n… truncated (" + strconv.Itoa(10000-len(head)) + " more characters)"
	if !strings.HasSuffix(got, wantNote) {
		t.Errorf("note wrong: tail %q, want suffix %q", got[len(got)-60:], wantNote)
	}

	multi := strings.Repeat("日本語é", 2000) // 8000 runes
	got = TruncateOutput(multi, 2900)
	if !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 after truncation")
	}
	if n := utf8.RuneCountInString(got); n > 2900 {
		t.Errorf("rune count = %d, want <= 2900", n)
	}

	lines := strings.Repeat(strings.Repeat("y", 49)+"\n", 100)
	got = TruncateOutput(lines, 1000)
	cut := got[:strings.Index(got, "\n… truncated (")]
	if !strings.HasSuffix(cut, "y") || strings.Contains(cut[len(cut)-5:], "\n") {
		t.Errorf("expected cut at a line break, got tail %q", cut[len(cut)-10:])
	}
	if utf8.RuneCountInString(cut) > 1000 || len(cut)%50 != 49 {
		t.Errorf("cut not on a line boundary: len %d", len(cut))
	}
}

func TestEscapeMrkdwn(t *testing.T) {
	if got := EscapeMrkdwn("a<b>&c"); got != "a&lt;b&gt;&amp;c" {
		t.Errorf("got %q", got)
	}
	for _, in := range []string{"x```y", "``````", "`````", "````"} {
		if got := EscapeMrkdwn(in); strings.Contains(got, "```") {
			t.Errorf("EscapeMrkdwn(%q) = %q still has three backticks", in, got)
		}
	}
}

func TestHeaderAndLinks(t *testing.T) {
	url := "https://dashboard.clearc2.com/d/host-diagnostics?var-host=host"
	h, links := HeaderAndLinks("host:9100 using > 75% RAM for longer than 10 minutes — "+url, "fb")
	if h != "host:9100 using > 75% RAM for longer than 10 minutes" {
		t.Errorf("header = %q", h)
	}
	if len(links) != 1 || links[0] != url {
		t.Errorf("links = %v", links)
	}
	h, _ = HeaderAndLinks(strings.Repeat("a", 400), "fb")
	if utf8.RuneCountInString(h) > 150 || !strings.HasSuffix(h, "…") {
		t.Errorf("long header: %d runes, %q", utf8.RuneCountInString(h), h[len(h)-5:])
	}
	if h, l := HeaderAndLinks("", "fb"); h != "fb" || len(l) != 0 {
		t.Errorf("empty: %q %v", h, l)
	}
	if h, _ := HeaderAndLinks("  - : https://x.example/a", "fb"); h != "fb" {
		t.Errorf("only-URL description: %q", h)
	}
}

func cmds(n int, output string) []CommandSection {
	out := make([]CommandSection, n)
	for i := range out {
		out[i] = CommandSection{Title: "t" + strconv.Itoa(i), Command: "c", Output: output}
	}
	return out
}

func TestBuildMessageShape(t *testing.T) {
	m := BuildMessage("desc", "fb", cmds(2, "out"), "", 2900)
	if len(m.Blocks) != 5 {
		t.Fatalf("blocks = %d, want 5", len(m.Blocks))
	}
	if m.Blocks[0].Type != "header" || m.Blocks[0].Text.Type != "plain_text" {
		t.Errorf("header block: %+v", m.Blocks[0])
	}
	if m.Blocks[1].Text.Text != "*t0*" || m.Blocks[1].Text.Type != "mrkdwn" {
		t.Errorf("title block: %+v", m.Blocks[1])
	}
	o := m.Blocks[2].Text.Text
	if !strings.HasPrefix(o, "```") || !strings.HasSuffix(o, "```") || !strings.Contains(o, "out") {
		t.Errorf("output block: %q", o)
	}
	m = BuildMessage("d", "fb", cmds(1, ""), "", 2900)
	if !strings.Contains(m.Blocks[2].Text.Text, "(no output)") {
		t.Errorf("empty output: %q", m.Blocks[2].Text.Text)
	}
}

func TestBuildMessageLinks(t *testing.T) {
	m := BuildMessage("d — https://a.example/x?a=1&b=2 https://b.example", "fb", nil, "", 2900)
	if len(m.Blocks) != 2 || !strings.Contains(m.Blocks[1].Text.Text, "https://a.example/x?a=1&amp;b=2") ||
		!strings.Contains(m.Blocks[1].Text.Text, "https://b.example") {
		t.Errorf("links block: %+v", m.Blocks)
	}
}

func TestBuildMessageTitleOnly(t *testing.T) {
	m := BuildMessage("desc", "fb", nil, "", 2900)
	if len(m.Blocks) != 1 || m.Blocks[0].Type != "header" {
		t.Errorf("blocks: %+v", m.Blocks)
	}
}

func TestBuildMessageNote(t *testing.T) {
	note := "No report could be collected from h1: connection refused"
	m := BuildMessage("desc", "fb", nil, note, 2900)
	if len(m.Blocks) != 2 || !strings.Contains(m.Blocks[1].Text.Text, note) || m.Blocks[1].Text.Type != "mrkdwn" {
		t.Errorf("blocks: %+v", m.Blocks)
	}
}

func TestBuildMessageBlockCap(t *testing.T) {
	m := BuildMessage("desc", "fb", cmds(40, "o"), "", 2900)
	if len(m.Blocks) != 50 {
		t.Fatalf("blocks = %d, want 50", len(m.Blocks))
	}
	last := m.Blocks[49].Text.Text
	if !strings.Contains(last, "16 more sections not shown") {
		t.Errorf("last block: %q", last)
	}
	m = BuildMessage("desc", "fb", cmds(24, "o"), "a note", 2900) // 1+1+48 = 50, fits exactly
	if len(m.Blocks) != 50 || strings.Contains(m.Blocks[49].Text.Text, "not shown") {
		t.Errorf("exact fit: %d blocks, last %q", len(m.Blocks), m.Blocks[len(m.Blocks)-1].Text.Text)
	}
	m = BuildMessage("desc", "fb", cmds(30, "o"), "a note", 2900)
	if len(m.Blocks) > 50 {
		t.Errorf("with note: %d blocks", len(m.Blocks))
	}
}

func checkLimits(t *testing.T, m SlackBlocks) {
	t.Helper()
	for i, b := range m.Blocks {
		limit := 3000
		if b.Type == "header" {
			limit = 150
		}
		if n := utf8.RuneCountInString(b.Text.Text); n > limit {
			t.Errorf("block %d has %d chars (limit %d)", i, n, limit)
		}
	}
}

func TestBuildMessageSectionLimit(t *testing.T) {
	m := BuildMessage("desc", "fb", cmds(1, strings.Repeat("z", 10000)), "", 2900)
	checkLimits(t, m)
}

func TestBuildMessageEscapedOutputFits(t *testing.T) {
	m := BuildMessage("desc", "fb", cmds(1, strings.Repeat("&", 2900)), "", 2900)
	checkLimits(t, m)
	o := m.Blocks[2].Text.Text
	if !strings.HasPrefix(o, "```") || !strings.HasSuffix(o, "```") || !strings.Contains(o, "truncated") {
		t.Errorf("output block shape: %q", o[:20])
	}
	r := strings.NewReplacer("&amp;", "", "&lt;", "", "&gt;", "")
	if strings.Contains(r.Replace(o), "&") {
		t.Errorf("broken entity in output")
	}
	// '<' and a multibyte mix must also stay valid and within limits
	m = BuildMessage("desc", "fb", cmds(1, strings.Repeat("<日", 3000)), "", 2900)
	checkLimits(t, m)
	if !utf8.ValidString(m.Blocks[2].Text.Text) {
		t.Errorf("invalid UTF-8")
	}
}

func TestBuildMessageKeepsClosingNote(t *testing.T) {
	const note = "(timed out after 20s)"
	output := strings.Repeat("&", 2900-1-len(note)) + "\n" + note // 2,900 characters, as a host sends it
	m := BuildMessage("desc", "fb", []CommandSection{{Title: "ps", Output: output}}, "", 2900)
	checkLimits(t, m)
	o := m.Blocks[2].Text.Text
	if !strings.HasSuffix(o, "\n"+note+"```") {
		t.Errorf("output block lost the closing note: tail %q", o[len(o)-60:])
	}
	if !strings.Contains(o, "truncated") {
		t.Errorf("output block does not say it was cut: tail %q", o[len(o)-60:])
	}

	// A host "truncated" line is kept the same way, also when the monitor's max_output_chars is lower.
	hostCut := strings.Repeat("line of ps output\n", 200) + "… truncated (8000 more characters)"
	m = BuildMessage("desc", "fb", []CommandSection{{Title: "ps", Output: hostCut}}, "", 1000)
	o = m.Blocks[2].Text.Text
	if !strings.HasSuffix(o, "\n… truncated (8000 more characters)```") || utf8.RuneCountInString(o) > 1006 {
		t.Errorf("host truncated line not kept: %d chars, tail %q", utf8.RuneCountInString(o), o[len(o)-60:])
	}

	// Output that fits is left alone, and a note that is not the last line gets no special care.
	short := "a & b\n" + note
	if o := BuildMessage("d", "fb", []CommandSection{{Output: short}}, "", 2900).Blocks[2].Text.Text; o != "```a &amp; b\n"+note+"```" {
		t.Errorf("short output changed: %q", o)
	}
}

func TestBuildMessageKeepsExitNoteWithStderrLine(t *testing.T) {
	const tail = "(exit status 3)\nboom"
	output := strings.Repeat("&", 2900-1-len(tail)) + "\n" + tail
	m := BuildMessage("desc", "fb", []CommandSection{{Title: "nginx -t", Output: output}}, "", 2900)
	checkLimits(t, m)
	o := m.Blocks[2].Text.Text
	if !strings.HasSuffix(o, "\n"+tail+"```") {
		t.Errorf("output block lost the exit note and stderr line: tail %q", o[len(o)-60:])
	}
	// A tail that cannot fit the block falls back to a plain cut, still within limits.
	long := strings.Repeat("x", 500) + "\n(exit status 1)\n" + strings.Repeat(">", 1000)
	m = BuildMessage("desc", "fb", []CommandSection{{Output: long}}, "", 2900)
	checkLimits(t, m)
}

func TestBuildMessageKeepsSignalNoteWithStderrLine(t *testing.T) {
	// The note text exactly as RunCommand writes it for a SIGKILL'd command.
	tail := fmt.Sprintf("(killed by signal %s)\nKilled", syscall.SIGKILL)
	output := strings.Repeat("&", 2900-1-len(tail)) + "\n" + tail
	m := BuildMessage("desc", "fb", []CommandSection{{Title: "du", Output: output}}, "", 2900)
	checkLimits(t, m)
	o := m.Blocks[2].Text.Text
	if !strings.HasSuffix(o, "\n"+tail+"```") {
		t.Errorf("output block lost the signal note and stderr line: tail %q", o[len(o)-60:])
	}
}

func TestBuildMessageLongTitleAndHeader(t *testing.T) {
	c := []CommandSection{{Title: strings.Repeat("&", 5000), Output: "x"}}
	m := BuildMessage(strings.Repeat("h", 500), "fb", c, "", 2900)
	checkLimits(t, m)
	r := strings.NewReplacer("&amp;", "")
	if strings.Contains(r.Replace(m.Blocks[1].Text.Text), "&") {
		t.Errorf("broken entity in title")
	}
}

func TestPostChecksStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, "invalid_blocks")
	}))
	defer srv.Close()
	p := &SlackPoster{}
	err := p.Post(context.Background(), srv.URL+"/hook/SECRET", SlackBlocks{})
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_blocks") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("URL leaked in error: %v", err)
	}
}

func TestPostDoesNotLeakURLOnTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL + "/hook/SECRET"
	srv.Close()
	err := (&SlackPoster{}).Post(context.Background(), url, SlackBlocks{})
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), url) {
		t.Fatalf("err = %v", err)
	}
	err = (&SlackPoster{}).Post(context.Background(), "http://bad host/hook/SECRET", SlackBlocks{})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("bad URL err = %v", err)
	}
}

func TestPostRetriesOnce(t *testing.T) {
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqs.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	var slept []time.Duration
	p := &SlackPoster{Sleep: func(d time.Duration) { slept = append(slept, d) }}
	if err := p.Post(context.Background(), srv.URL, SlackBlocks{}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if reqs.Load() != 2 || len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("reqs=%d slept=%v", reqs.Load(), slept)
	}

	reqs.Store(0)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		w.WriteHeader(500)
	}))
	defer srv2.Close()
	slept = nil
	err := p.Post(context.Background(), srv2.URL, SlackBlocks{})
	if err == nil || !strings.Contains(err.Error(), "500") || reqs.Load() != 2 {
		t.Errorf("err=%v reqs=%d", err, reqs.Load())
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Errorf("default wait: %v", slept)
	}
}

func TestPostRetryAfterCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	var slept []time.Duration
	p := &SlackPoster{Sleep: func(d time.Duration) { slept = append(slept, d) }}
	_ = p.Post(context.Background(), srv.URL, SlackBlocks{})
	if len(slept) != 1 || slept[0] != 30*time.Second {
		t.Errorf("slept %v, want [30s]", slept)
	}
}

// hangFirst is a fake Slack whose first request hangs (until the client gives up or 10s pass) and
// whose later requests succeed.
func hangFirst(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqs.Add(1) == 1 {
			io.Copy(io.Discard, r.Body) // after the body, the server notices the client leaving
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestPostTimeoutPerAttempt(t *testing.T) {
	srv, reqs := hangFirst(t)
	p := &SlackPoster{Timeout: 300 * time.Millisecond, Sleep: func(time.Duration) {}}
	start := time.Now()
	if err := p.Post(context.Background(), srv.URL+"/hook/SECRET", SlackBlocks{}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 3*time.Second {
		t.Errorf("Post took %v, want about the 300ms timeout", d)
	}
	if reqs.Load() != 2 {
		t.Errorf("requests = %d, want 2", reqs.Load())
	}
}

func TestPostStatusErrorCarriesCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "invalid_blocks")
	}))
	defer srv.Close()
	err := (&SlackPoster{}).Post(context.Background(), srv.URL+"/hook/SECRET", SlackBlocks{})
	var se *SlackStatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusBadRequest || se.Body != "invalid_blocks" {
		t.Fatalf("err = %#v, want a *SlackStatusError with 400 and the reply", err)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("URL leaked in error: %v", err)
	}
}

func TestPostWaitEndsWhenContextDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := (&SlackPoster{}).Post(ctx, srv.URL, SlackBlocks{})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("err=%v after %v", err, time.Since(start))
	}
}
