package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maxStdoutBytes   = 64 << 10 // stdout kept per command; the rest is drained and discarded
	maxStderrBytes   = 4 << 10  // stderr tail kept, enough for its last line
	maxStderrLine    = 200      // longest stderr line shown in the output
	commandWaitDelay = 2 * time.Second
	budgetSkipped    = "(skipped: report time budget used up)"
	// reportWriteMargin is added to the report budget for the /report reply's write deadline: room
	// for the per-command kill grace (commandWaitDelay) and for sending the reply.
	reportWriteMargin = 30 * time.Second
)

// headWriter keeps the first max bytes written and silently discards the rest, so the
// command never blocks on a full pipe.
type headWriter struct {
	mu       sync.Mutex
	buf      []byte
	max      int
	overflow bool
}

func (w *headWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.max - len(w.buf); room > 0 {
		if len(p) > room {
			w.buf = append(w.buf, p[:room]...)
			w.overflow = true
		} else {
			w.buf = append(w.buf, p...)
		}
	} else if len(p) > 0 {
		w.overflow = true
	}
	return len(p), nil
}

// tailWriter keeps the last max bytes written.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if extra := len(w.buf) - w.max; extra > 0 {
		w.buf = append(w.buf[:0], w.buf[extra:]...)
	}
	return len(p), nil
}

// lastLine returns the last non-empty line, cut to maxStderrLine bytes.
func (w *tailWriter) lastLine() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := strings.TrimSpace(string(w.buf))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	return cutRunes(s, maxStderrLine)
}

// cutRunes cuts s to at most n bytes without splitting a UTF-8 sequence.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// formatTimeout prints a timeout of 1s or more rounded to whole seconds ("Ns"); a budget-clipped
// timeout such as 14.998231407s prints "15s". Sub-second timeouts print Go's duration text; they only
// come from tests calling RunCommand directly, since settings give whole seconds and RunReport skips
// a command with less than one second of budget left.
func formatTimeout(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%ds", int64(d.Round(time.Second)/time.Second))
	}
	return d.String()
}

// RunCommand runs `bash -c command` in its own process group with LC_ALL=C. When the timeout
// passes or ctx ends, the whole group is killed. The result is the trimmed stdout (what was
// printed so far is kept), followed by a note on a non-zero exit or a timeout, at most maxChars characters.
func RunCommand(ctx context.Context, command string, timeout time.Duration, maxChars int) string {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout := &headWriter{max: maxStdoutBytes}
	stderr := &tailWriter{max: maxStderrBytes}
	cmd := exec.CommandContext(cmdCtx, "bash", "-c", command)
	cmd.Env = append(os.Environ(), "LC_ALL=C") // exec keeps the last of duplicate keys
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var killed atomic.Bool
	cmd.Cancel = func() error {
		// The group id is bash's pid (Setpgid). Kill the group, not just bash, so children die too.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		killed.Store(true)
		return nil
	}
	// A child that left the group could hold the pipes open; do not let it hang Wait.
	cmd.WaitDelay = commandWaitDelay

	err := cmd.Run()
	if cmd.Process != nil {
		// Kill anything bash left behind in its group (e.g. `cmd &` without wait). The group id
		// cannot be reused while any member is alive; ESRCH just means nothing was left.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	var suffix string
	var exitErr *exec.ExitError
	switch {
	case err == nil || errors.Is(err, exec.ErrWaitDelay):
		// Exited 0; ErrWaitDelay means a leftover child held stdout open after bash exited.
	case killed.Load() && errors.Is(cmdCtx.Err(), context.DeadlineExceeded):
		suffix = fmt.Sprintf("\n(timed out after %s)", formatTimeout(timeout))
	case cmdCtx.Err() != nil:
		suffix = "\n(canceled)"
	case errors.As(err, &exitErr):
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			suffix = fmt.Sprintf("\n(killed by signal %s)", ws.Signal())
		} else {
			suffix = fmt.Sprintf("\n(exit status %d)", exitErr.ExitCode())
		}
		if line := stderr.lastLine(); line != "" {
			suffix += "\n" + line
		}
	default:
		suffix = fmt.Sprintf("\n(could not run: %v)", err)
	}

	stdout.mu.Lock()
	out := strings.TrimSpace(string(stdout.buf))
	overflow := stdout.overflow
	stdout.mu.Unlock()
	if overflow {
		out += "\n… truncated (output over 64 KiB)"
	}
	if out == "" {
		return headRunes(strings.TrimPrefix(suffix, "\n"), maxChars)
	}
	// Cut stdout, not the note, so the exit or timeout note shows. A note longer than maxChars
	// itself is cut, so the result never exceeds maxChars.
	room := maxChars - utf8.RuneCountInString(suffix)
	if room <= 0 {
		return headRunes(strings.TrimPrefix(suffix, "\n"), maxChars)
	}
	return TruncateOutput(out, room) + suffix
}

// RunReport runs an alert's commands one after another within cfg.ReportBudgetSeconds and returns
// copies with Output filled; cfg is never written. A command reached with less than one second of
// budget left is skipped.
func RunReport(ctx context.Context, cfg *Config, alertName string) []CommandSection {
	deadline := time.Now().Add(time.Duration(cfg.ReportBudgetSeconds) * time.Second)
	sections := make([]CommandSection, 0, len(cfg.Alerts[alertName]))
	for _, c := range cfg.Alerts[alertName] {
		if c == nil {
			continue // a `null` entry in the settings file
		}
		s := CommandSection{Title: c.Title, Command: c.Command, Timeout: c.Timeout}
		timeout := time.Duration(cfg.CommandTimeoutSeconds) * time.Second
		if c.Timeout > 0 {
			timeout = time.Duration(c.Timeout) * time.Second
		}
		left := time.Until(deadline)
		if left < time.Second {
			s.Output = budgetSkipped
		} else {
			s.Output = RunCommand(ctx, c.Command, min(timeout, left), cfg.MaxOutputChars)
		}
		sections = append(sections, s)
	}
	return sections
}

// validToken reports whether the request's token query parameter matches cfg.Token, comparing in
// constant time. A failure is logged with the remote address, never with either token.
func validToken(cfg *Config, r *http.Request) bool {
	got := r.URL.Query().Get("token")
	if cfg.Token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Token)) == 1 {
		return true
	}
	log.Printf("rejected %s %s: invalid token from %s", r.Method, r.URL.Path, r.RemoteAddr)
	return false
}

// reportEntry is one command in the /report reply: exactly title, command and output.
type reportEntry struct {
	Title   string `json:"title"`
	Command string `json:"command"`
	Output  string `json:"output"`
}

type reportReply struct {
	Commands []reportEntry `json:"commands"`
}

// reportHandler serves GET /report?token=…&alertName=…: it runs the alert's commands and replies
// {"commands":[{"title","command","output"}]} (an unknown alert gives an empty list).
func reportHandler(store *ConfigStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		cfg := store.Get()
		if !validToken(cfg, r) {
			writeError(w, "Invalid token")
			return
		}
		alertName := r.URL.Query().Get("alertName")
		if alertName == "" {
			writeError(w, "Missing alert name")
			return
		}
		// The server's WriteTimeout is fixed; give this reply as long as the budget allows instead.
		deadline := time.Now().Add(time.Duration(cfg.ReportBudgetSeconds)*time.Second + reportWriteMargin)
		if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			log.Printf("report %q: cannot set write deadline: %v", alertName, err)
		}
		sections := RunReport(r.Context(), cfg, alertName)
		reply := reportReply{Commands: make([]reportEntry, 0, len(sections))}
		for _, s := range sections {
			reply.Commands = append(reply.Commands, reportEntry{Title: s.Title, Command: s.Command, Output: s.Output})
		}
		payload, err := json.Marshal(reply)
		if err != nil {
			writeError(w, "Could not encode command output")
			return
		}
		writeJson(w, payload)
	}
}
