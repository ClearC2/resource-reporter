package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// CommandSection is one command of an alert's report. Output is filled in when the command runs.
type CommandSection struct {
	Title   string `json:"title"`
	Command string `json:"command"`
	Output  string `json:"output"`
	Timeout int    `json:"timeout,omitempty"` // seconds; 0 means the global command timeout
}

// Config is the settings file. Every key beyond slack, token, alerts and hosts is optional.
type Config struct {
	Slack  string                       `json:"slack"`
	Token  string                       `json:"token"`
	Alerts map[string][]*CommandSection `json:"alerts"`
	Hosts  map[string]string            `json:"hosts"`

	// Pointers so an explicit 0 / false differs from "absent".
	RepeatHours       *float64 `json:"repeat_hours"`
	UnreachableNotice *bool    `json:"unreachable_notice"`

	CommandTimeoutSeconds int `json:"command_timeout_seconds"`
	ReportBudgetSeconds   int `json:"report_budget_seconds"`
	HostTimeoutSeconds    int `json:"host_timeout_seconds"`
	SlackTimeoutSeconds   int `json:"slack_timeout_seconds"`
	MaxOutputChars        int `json:"max_output_chars"`

	// Monitor side: when set, only hosts matching an entry (or named in Hosts) are asked for reports.
	ReportHosts []string `json:"report_hosts"`
}

// reportHostRule is one parsed report_hosts entry: exactly one field is set.
type reportHostRule struct {
	name   string     // exact host name, lower case
	suffix string     // ".example.com", lower case
	ipnet  *net.IPNet // a CIDR, or a single IP address
}

// parseReportHost parses a report_hosts entry: an exact host name, a suffix starting with ".", a
// CIDR, or a single IP address.
func parseReportHost(entry string) (reportHostRule, error) {
	e := strings.ToLower(entry)
	cidr := e
	if ip := net.ParseIP(e); ip != nil {
		cidr = e + "/128"
		if ip.To4() != nil {
			cidr = e + "/32"
		}
	}
	switch {
	case strings.Contains(cidr, "/"):
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			return reportHostRule{ipnet: n}, nil
		}
	case strings.HasPrefix(e, "."):
		if nameChars(e[1:]) {
			return reportHostRule{suffix: e}, nil
		}
	case nameChars(e):
		return reportHostRule{name: e}, nil
	}
	return reportHostRule{}, fmt.Errorf("settings: report_hosts entry %q is not a host name, a .suffix or a CIDR", entry)
}

// nameChars reports whether s is a non-empty run of letters, digits, ".", "-" and "_".
func nameChars(s string) bool {
	return hostChars(s) && !strings.ContainsAny(s, "[]:")
}

// ReportHostAllowed reports whether the monitor may ask host for a report: always when report_hosts
// is absent or empty, otherwise only when host matches an entry (names compare case-insensitively;
// a CIDR matches a host that is an IP literal). Hosts from the hosts map are not checked here.
func (c *Config) ReportHostAllowed(host string) bool {
	if len(c.ReportHosts) == 0 {
		return true
	}
	h := strings.ToLower(host)
	ip := net.ParseIP(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]"))
	for _, entry := range c.ReportHosts {
		rule, err := parseReportHost(entry)
		switch {
		case err != nil: // LoadConfig refuses such settings
		case rule.ipnet != nil:
			if ip != nil && rule.ipnet.Contains(ip) {
				return true
			}
		case rule.suffix != "":
			if len(h) > len(rule.suffix) && strings.HasSuffix(h, rule.suffix) {
				return true
			}
		case h == rule.name:
			return true
		}
	}
	return false
}

const (
	defaultRepeatHours           = 6.0
	defaultCommandTimeoutSeconds = 20
	defaultReportBudgetSeconds   = 75
	defaultHostTimeoutSeconds    = 90
	defaultSlackTimeoutSeconds   = 10
	defaultMaxOutputChars        = 2900
)

func (c *Config) applyDefaults() {
	if c.RepeatHours == nil {
		h := defaultRepeatHours
		c.RepeatHours = &h
	}
	if c.UnreachableNotice == nil {
		b := true
		c.UnreachableNotice = &b
	}
	if c.CommandTimeoutSeconds <= 0 {
		c.CommandTimeoutSeconds = defaultCommandTimeoutSeconds
	}
	if c.ReportBudgetSeconds <= 0 {
		c.ReportBudgetSeconds = defaultReportBudgetSeconds
	}
	if c.HostTimeoutSeconds <= 0 {
		c.HostTimeoutSeconds = defaultHostTimeoutSeconds
	}
	if c.SlackTimeoutSeconds <= 0 {
		c.SlackTimeoutSeconds = defaultSlackTimeoutSeconds
	}
	if c.MaxOutputChars <= 0 {
		c.MaxOutputChars = defaultMaxOutputChars
	}
}

// RepeatEvery is how long a repeated alert stays quiet; 0 (or negative) means no repeat memory.
func (c *Config) RepeatEvery() time.Duration {
	if c.RepeatHours == nil || *c.RepeatHours <= 0 {
		return 0
	}
	return time.Duration(*c.RepeatHours * float64(time.Hour))
}

// NoticeUnreachable reports whether a host that cannot be reached gets a Slack notice.
func (c *Config) NoticeUnreachable() bool {
	return c.UnreachableNotice == nil || *c.UnreachableNotice
}

// LoadConfig reads and validates the settings file, applying defaults for absent keys.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.Token == "" {
		return nil, errors.New("settings: token is empty")
	}
	for _, entry := range c.ReportHosts {
		if _, err := parseReportHost(entry); err != nil {
			return nil, err
		}
	}
	c.applyDefaults()
	return &c, nil
}

// ConfigStore serves the current settings, re-reading the file when it changes and
// keeping the last good settings when the new content is unreadable or invalid.
type ConfigStore struct {
	path string

	mu      sync.Mutex
	current *Config
	modTime time.Time
	size    int64
}

// NewConfigStore loads the settings once; it fails if that first load fails.
func NewConfigStore(path string) (*ConfigStore, error) {
	s := &ConfigStore{path: path}
	// Stat before reading so a write in between triggers a re-read on the next Get.
	info, statErr := os.Stat(path)
	c, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	s.current = c
	if statErr == nil {
		s.modTime, s.size = info.ModTime(), info.Size()
	}
	return s, nil
}

// Get returns the current settings. Safe for concurrent use; callers must not modify the result.
func (s *ConfigStore) Get() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(s.path)
	if err != nil {
		log.Printf("settings: cannot stat file, keeping last good settings: %v", err)
		return s.current
	}
	if info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return s.current
	}
	// Remember this version even if it fails, so a bad file is logged once, not on every request.
	s.modTime, s.size = info.ModTime(), info.Size()
	c, err := LoadConfig(s.path)
	if err != nil {
		log.Printf("settings: reload failed, keeping last good settings: %v", err)
		return s.current
	}
	s.current = c
	log.Printf("settings: reloaded")
	return s.current
}
