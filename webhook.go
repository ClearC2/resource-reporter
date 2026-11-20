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
// result is empty or cannot be a host name (it contains "/", "@", "?", "#", a space or "{").
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
	if host == "" || strings.ContainsAny(host, "/@?# {") {
		return "", false
	}
	return host, true
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

// SeenAlerts remembers when each alert was last reported. Safe for concurrent use.
type SeenAlerts struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func NewSeenAlerts() *SeenAlerts {
	return &SeenAlerts{last: make(map[string]time.Time)}
}

// ShouldReport reports whether the alert with this key is due: it is new or was last reported at
// least every ago. A true answer records now, so of two simultaneous callers only one gets true.
// every <= 0 always reports. Entries older than 2*every are dropped.
func (s *SeenAlerts) ShouldReport(key string, now time.Time, every time.Duration) bool {
	if every <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.last {
		if now.Sub(t) > 2*every {
			delete(s.last, k)
		}
	}
	if t, seen := s.last[key]; seen && now.Sub(t) < every {
		return false
	}
	s.last[key] = now
	return true
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
	key := host + "|" + name + "|" + a.StartsAt
	if !r.Seen.ShouldReport(key, r.Now(), cfg.RepeatEvery()) {
		return
	}

	fallback := name + " on " + host
	commands, reason := r.fetchReport(ctx, cfg, host, name)
	var msg SlackBlocks
	if reason == "" {
		msg = BuildMessage(a.Annotations.Description, fallback, commands, "", cfg.MaxOutputChars)
	} else {
		log.Printf("alert %q: no report from %s: %s", name, host, reason)
		if !cfg.NoticeUnreachable() {
			// Nothing is posted, so let Alertmanager's next notification try again.
			r.Seen.Forget(key)
			return
		}
		note := fmt.Sprintf("No report could be collected from %s: %s", host, reason)
		msg = BuildMessage(a.Annotations.Description, fallback, nil, note, cfg.MaxOutputChars)
	}

	slackCtx, cancel := context.WithTimeout(ctx, time.Duration(2*cfg.SlackTimeoutSeconds+32)*time.Second)
	defer cancel()
	if err := r.Poster.Post(slackCtx, cfg.Slack, msg); err != nil {
		// Post's errors never contain the webhook URL. Forget the alert so the next notification retries.
		log.Printf("alert %q on %s: %v", name, host, err)
		r.Seen.Forget(key)
		return
	}
	log.Printf("alert %q on %s: reported", name, host)
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHostReply))
	if err != nil {
		if ctx.Err() != nil {
			return nil, "timed out"
		}
		return nil, "unreadable reply"
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
