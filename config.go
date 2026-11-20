package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
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

// getConfig is a shim for the 2022 handlers in resource-reporter.go; Task 6 deletes it.
func getConfig() (*Config, error) {
	return LoadConfig(os.Args[1])
}
