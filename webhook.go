package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxHostReply   = 1 << 20  // bytes of a host's /report reply that are read
	maxWebhookBody = 10 << 20 // bytes of an Alertmanager notification that are read
)

// Alert is one alert of an Alertmanager webhook notification (only the fields used here).
type Alert struct {
	Status string `json:"status"`
	Labels struct {
		Alertname string `json:"alertname"`
		Instance  string `json:"instance"`
	} `json:"labels"`
	Annotations struct {
		Description string `json:"description"`
	} `json:"annotations"`
	StartsAt string `json:"startsAt"`
}

// WebhookPayload is an Alertmanager webhook notification.
type WebhookPayload struct {
	Status      string  `json:"status"`
	Alerts      []Alert `json:"alerts"`
	ExternalURL string  `json:"externalURL"`
}

// AlertHost returns the host to ask for a report: the hosts map entry for the full instance value
// when there is one, otherwise the instance without its trailing ":port". ok is false when the
// result is empty or cannot be a host name (see hostChars).
func AlertHost(cfg *Config, a Alert) (host string, ok bool) {
	instance := a.Labels.Instance
	if mapped := cfg.Hosts[instance]; mapped != "" {
		host = mapped
	} else {
		host = instance
		if i := strings.LastIndexByte(instance, ':'); i >= 0 && isPort(instance[i+1:]) {
			host = instance[:i]
		}
	}
	if !hostChars(host) {
		return "", false
	}
	return host, true
}

// hostChars reports whether s is non-empty and holds only ASCII letters, digits, ".", "-", "_",
// and "[", "]", ":" (for IPv6 literals).
func hostChars(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.', c == '-', c == '_', c == '[', c == ']', c == ':':
		default:
			return false
		}
	}
	return true
}

func isPort(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// seenAlert is the repeat memory's record of one alert.
type seenAlert struct {
	at         time.Time // when the current window started
	noticeOnly bool      // only a "no report could be collected" notice has been posted
	busy       bool      // a caller holds the claim and has not settled it yet
}

// SeenAlerts remembers which alerts were reported, and whether that was a real report or only a
// notice. Safe for concurrent use.
type SeenAlerts struct {
	mu   sync.Mutex
	last map[string]*seenAlert
}

func NewSeenAlerts() *SeenAlerts {
	return &SeenAlerts{last: make(map[string]*seenAlert)}
}

// ShouldReport claims the alert with this key when it is due. ok is true for a fresh claim (the
// alert is new or its window of every has passed) and for a retry (inside the window only a notice
// was posted, so the host is asked again); retry tells the two apart. A claim is held until the
// caller settles it with MarkReported, MarkNotice, Release or Forget, so of two simultaneous callers
// only one gets ok. every <= 0 always gives a fresh claim. Entries older than 2*every are dropped.
func (s *SeenAlerts) ShouldReport(key string, now time.Time, every time.Duration) (ok, retry bool) {
	if every <= 0 {
		return true, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.last {
		if now.Sub(e.at) > 2*every {
			delete(s.last, k)
		}
	}
	e, seen := s.last[key]
	if !seen || now.Sub(e.at) >= every {
		s.last[key] = &seenAlert{at: now, busy: true}
		return true, false
	}
	if e.noticeOnly && !e.busy {
		e.busy = true
		return true, true
	}
	return false, false
}

// MarkReported settles a claim: a real report was posted, and the window restarts at now.
func (s *SeenAlerts) MarkReported(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.last[key]; e != nil {
		*e = seenAlert{at: now}
	}
}

// MarkNotice settles a fresh claim: only a notice was posted, so later notifications inside the
// window ask the host again (without a second notice).
func (s *SeenAlerts) MarkNotice(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.last[key]; e != nil {
		e.noticeOnly, e.busy = true, false
	}
}

// Release settles a retry claim that posted nothing: the alert stays notice-only.
func (s *SeenAlerts) Release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.last[key]; e != nil {
		e.busy = false
	}
}

// Forget drops the record for key, so the next notification for that alert reports again.
func (s *SeenAlerts) Forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.last, key)
}

// Reporter turns firing alerts into Slack messages: it asks the alert's host for a report and posts
// it. Build it with NewReporter; the fields may be replaced before use (tests do).
type Reporter struct {
	Store     *ConfigStore
	Seen      *SeenAlerts
	HTTP      *http.Client // for the host requests
	Poster    *SlackPoster
	ReportURL func(host string) string // base URL of a host's /report; token and alertName are added
	Now       func() time.Time

	wg sync.WaitGroup
}

func NewReporter(store *ConfigStore) *Reporter {
	return &Reporter{
		Store: store,
		Seen:  NewSeenAlerts(),
		HTTP: &http.Client{
			// A redirect is not a report; following it would also send the token elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Poster:    &SlackPoster{},
		ReportURL: func(host string) string { return "http://" + host + ":5050/report" },
		Now:       time.Now,
	}
}

// ProcessAlert reports one alert to Slack: the host's report, or the title with a note saying why no
// report could be collected. Neither log lines nor notes ever contain the token or the request URL.
func (r *Reporter) ProcessAlert(ctx context.Context, a Alert) {
	cfg := r.Store.Get()
	name := a.Labels.Alertname
	if cfg.Slack == "" {
		log.Printf("alert %q: no slack URL in settings, not reporting", name)
		return
	}
	host, ok := AlertHost(cfg, a)
	if !ok {
		log.Printf("alert %q: instance %q is not a host, skipped", name, a.Labels.Instance)
		return
	}
	if cfg.Hosts[a.Labels.Instance] == "" && !cfg.ReportHostAllowed(host) {
		// The request carries the fleet token: never send it to a host nobody listed.
		log.Printf("alert %q: host %q is not in report_hosts, skipped", name, host)
		return
	}
	key := host + "|" + name + "|" + a.StartsAt
	claimed, retry := r.Seen.ShouldReport(key, r.Now(), cfg.RepeatEvery())
	if !claimed {
		return
	}
	// giveUp settles a claim that posted nothing: a fresh claim is forgotten so the next notification
	// tries again; a retry stays notice-only (its notice is already in Slack).
	giveUp := func() {
		if retry {
			r.Seen.Release(key)
		} else {
			r.Seen.Forget(key)
		}
	}

	fallback := name + " on " + host
	commands, reason := r.fetchReport(ctx, cfg, host, name)
	if reason != "" {
		log.Printf("alert %q: no report from %q: %s", name, host, reason)
		if retry || !cfg.NoticeUnreachable() {
			// No (second) notice: let Alertmanager's next notification ask the host again.
			giveUp()
			return
		}
		note := fmt.Sprintf("No report could be collected from %s: %s", host, reason)
		if err := r.post(ctx, cfg, BuildMessage(a.Annotations.Description, fallback, nil, note, cfg.MaxOutputChars)); err != nil {
			log.Printf("alert %q on %q: %v", name, host, err)
			giveUp()
			return
		}
		r.Seen.MarkNotice(key)
		log.Printf("alert %q on %q: notice posted", name, host)
		return
	}

	msg := BuildMessage(a.Annotations.Description, fallback, commands, "", cfg.MaxOutputChars)
	if err := r.post(ctx, cfg, msg); err != nil {
		log.Printf("alert %q on %q: %v", name, host, err)
		giveUp()
		return
	}
	r.Seen.MarkReported(key, r.Now())
	log.Printf("alert %q on %q: reported", name, host)
}

// post sends msg to the settings' Slack webhook. Its errors never contain the webhook URL.
func (r *Reporter) post(ctx context.Context, cfg *Config, msg SlackBlocks) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(2*cfg.SlackTimeoutSeconds+32)*time.Second)
	defer cancel()
	return r.Poster.Post(ctx, cfg.Slack, msg)
}

// hostReply is the /report reply; a missing or null "commands" means no commands.
type hostReply struct {
	Commands []CommandSection `json:"commands"`
}

// fetchReport asks host for the alert's report. On failure it returns a short reason that never
// contains the request URL (which carries the token).
func (r *Reporter) fetchReport(ctx context.Context, cfg *Config, host, alertName string) ([]CommandSection, string) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.HostTimeoutSeconds)*time.Second)
	defer cancel()
	u, err := url.Parse(r.ReportURL(host))
	if err != nil {
		return nil, "invalid report URL"
	}
	q := u.Query()
	q.Set("token", cfg.Token)
	q.Set("alertName", alertName)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "invalid report URL"
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, shortReason(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Sprintf("host replied %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHostReply+1)) // one more byte shows a cut
	if err != nil {
		if ctx.Err() != nil {
			return nil, "timed out"
		}
		return nil, "unreadable reply"
	}
	if len(body) > maxHostReply {
		return nil, "reply too large"
	}
	var reply *hostReply // stays nil for a JSON null
	if err := json.Unmarshal(body, &reply); err != nil || reply == nil {
		return nil, "unreadable reply"
	}
	return reply.Commands, ""
}

// shortReason reduces a request error to a few words. The error itself is never shown: a
// *url.Error carries the full URL.
func shortReason(err error) string {
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "host unreachable"
	case errors.Is(err, syscall.ENETUNREACH):
		return "network unreachable"
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			return "no such host"
		}
		return "name lookup failed"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timed out"
	}
	return "request failed"
}

// webhookHandler serves POST /webhook?token=…: it answers at once and reports each firing alert
// in its own goroutine.
func (r *Reporter) webhookHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		cfg := r.Store.Get()
		if !validToken(cfg, req) {
			writeError(w, "Invalid token")
			return
		}
		var notification WebhookPayload
		body, err := io.ReadAll(io.LimitReader(req.Body, maxWebhookBody))
		if err != nil || json.Unmarshal(body, &notification) != nil {
			writeError(w, "Could not parse payload")
			return
		}
		timeout := time.Duration(cfg.HostTimeoutSeconds+2*cfg.SlackTimeoutSeconds+35) * time.Second
		for _, a := range notification.Alerts {
			if a.Status != "firing" {
				continue
			}
			r.wg.Add(1)
			go r.runAlert(a, timeout)
		}
		w.WriteHeader(http.StatusOK)
	}
}

// runAlert processes one alert (its own copy) and keeps a panic from stopping the service.
func (r *Reporter) runAlert(a Alert, timeout time.Duration) {
	defer r.wg.Done()
	defer func() {
		if p := recover(); p != nil {
			log.Printf("alert %q: panic while reporting: %v", a.Labels.Alertname, p)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	r.ProcessAlert(ctx, a)
}

// Wait blocks until every alert goroutine started by the handler has ended.
func (r *Reporter) Wait() {
	r.wg.Wait()
}
