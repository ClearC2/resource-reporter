package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Slack limits, in characters (runes) of the text as sent.
const (
	slackHeaderMax  = 150
	slackSectionMax = 3000
	slackBlocksMax  = 50

	lineBreakWindow = 200 // TruncateOutput looks this far back for a line break
	maxReplyRead    = 4 << 10
	maxReplyShown   = 200
	retryDefault    = 2 * time.Second
	retryCap        = 30 * time.Second
	zeroWidthSpace  = "\u200b"
)

type SlackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type SlackBlock struct {
	Type string    `json:"type"`
	Text SlackText `json:"text"`
}

type SlackBlocks struct {
	Blocks []SlackBlock `json:"blocks"`
}

// headRunes returns the first n runes of s.
func headRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// TruncateOutput keeps the head of s, at most max runes in all. Longer text is cut at a line break
// when one lies in the last 200 characters of what is kept, and "\n… truncated (N more characters)"
// is appended. A max too small for the note gets a plain cut.
func TruncateOutput(s string, max int) string {
	total := utf8.RuneCountInString(s)
	if total <= max {
		return s
	}
	// Size the note for the largest possible N so the real one never makes the result longer.
	keep := max - utf8.RuneCountInString(fmt.Sprintf("\n… truncated (%d more characters)", total))
	if keep <= 0 {
		return headRunes(s, max)
	}
	head := headRunes(s, keep)
	if i := strings.LastIndexByte(head, '\n'); i >= 0 &&
		utf8.RuneCountInString(head[i:]) <= lineBreakWindow {
		head = head[:i]
	}
	return head + fmt.Sprintf("\n… truncated (%d more characters)", total-utf8.RuneCountInString(head))
}

var backtickRun = regexp.MustCompile("`{3,}")

var mrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// EscapeMrkdwn escapes the three characters Slack treats specially and breaks any run of three
// backticks with zero-width spaces so output cannot close its own code block.
func EscapeMrkdwn(s string) string {
	return backtickRun.ReplaceAllStringFunc(mrkdwnEscaper.Replace(s), func(run string) string {
		return strings.ReplaceAll(run, "`", "`"+zeroWidthSpace)
	})
}

var urlPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

// HeaderAndLinks splits an alert description into a header (the text before the first URL, cut to
// 150 characters) and the URLs it contains. An empty header becomes fallback.
func HeaderAndLinks(description, fallback string) (header string, links []string) {
	text := description
	if loc := urlPattern.FindStringIndex(description); loc != nil {
		text = description[:loc[0]]
		for _, l := range urlPattern.FindAllString(description, -1) {
			links = append(links, strings.TrimRight(l, ".,;)"))
		}
	}
	header = strings.TrimRight(strings.TrimSpace(text), " -–—:\t\r\n")
	header = strings.TrimSpace(header)
	if header == "" {
		header = fallback
	}
	if utf8.RuneCountInString(header) > slackHeaderMax {
		header = headRunes(header, slackHeaderMax-1) + "…"
	}
	return header, links
}

// fitEscaped returns the escaped form of the longest cut(raw, m) whose escaped length is at most
// limit runes. Cutting happens before escaping, so an entity is never split.
func fitEscaped(raw string, limit int, cut func(string, int) string) string {
	fits := func(m int) (string, bool) {
		e := EscapeMrkdwn(cut(raw, m))
		return e, utf8.RuneCountInString(e) <= limit
	}
	hi := utf8.RuneCountInString(raw)
	if e, ok := fits(hi); ok {
		return e
	}
	lo, best := 0, ""
	for lo < hi { // invariant: lo fits (or is 0), hi does not
		mid := (lo + hi + 1) / 2
		if e, ok := fits(mid); ok {
			lo, best = mid, e
		} else {
			hi = mid - 1
		}
	}
	return best
}

func cutWithEllipsis(s string, m int) string {
	if utf8.RuneCountInString(s) <= m {
		return s
	}
	if m <= 1 {
		return "…"
	}
	return headRunes(s, m-1) + "…"
}

func mrkdwnBlock(text string) SlackBlock {
	return SlackBlock{Type: "section", Text: SlackText{Type: "mrkdwn", Text: text}}
}

// closingNote matches the status lines a host puts at the end of a command's output.
var closingNote = regexp.MustCompile(`^(\((timed out after |exit status |killed by signal |skipped: |could not run: ).*\)|\(canceled\)|… truncated \(.*\))$`)

// exitNote matches the "(exit status N)" line RunCommand puts before a failed command's last stderr line.
var exitNote = regexp.MustCompile(`^\(exit status [0-9]+\)$`)

// splitClosingNote splits output into the text before its closing note and the note: the last line
// when it is a host's status note or "… truncated" line, or the last two lines when the first of
// them is "(exit status N)" (followed by the command's last stderr line).
func splitClosingNote(output string) (head, note string, ok bool) {
	i := strings.LastIndexByte(output, '\n')
	if i < 0 {
		return "", "", false
	}
	if closingNote.MatchString(output[i+1:]) {
		return output[:i], output[i+1:], true
	}
	if j := strings.LastIndexByte(output[:i], '\n'); j >= 0 && exitNote.MatchString(output[j+1:i]) {
		return output[:j], output[j+1:], true
	}
	return "", "", false
}

// outputBlockText returns the fenced, escaped output so the whole text is at most 3,000 characters.
// When the output must be cut and ends in a host's status note (see splitClosingNote), the note is
// kept and the text before it is cut; a note too long for the block falls back to a plain cut.
func outputBlockText(output string, maxChars int) string {
	if strings.TrimSpace(output) == "" {
		return "```(no output)```"
	}
	const room = slackSectionMax - 6 - 2 // fences, and a space for each backtick at an edge
	cut := func(s string, m int) string { return TruncateOutput(s, min(m, maxChars)) }
	if head, note, ok := splitClosingNote(output); ok {
		keep := utf8.RuneCountInString(note) + 1 // the note and its line break
		cut = func(s string, m int) string {
			m = min(m, maxChars)
			if m <= keep || utf8.RuneCountInString(s) <= m {
				return TruncateOutput(s, m)
			}
			return TruncateOutput(head, m-keep) + "\n" + note
		}
	}
	body := fitEscaped(output, room, cut)
	if body == "" {
		body = "(output omitted)"
	}
	if strings.HasPrefix(body, "`") {
		body = zeroWidthSpace + body
	}
	if strings.HasSuffix(body, "`") {
		body += zeroWidthSpace
	}
	return "```" + body + "```"
}

// BuildMessage builds the Slack message for one alert: a header, the links, an optional note, then a
// title and fenced output per command. Every text respects Slack's limits as sent (after escaping),
// output is cut to maxChars first, and there are never more than 50 blocks; commands that do not
// fit are dropped and the last block says how many.
func BuildMessage(description, fallback string, commands []CommandSection, note string, maxChars int) SlackBlocks {
	header, links := HeaderAndLinks(description, fallback)
	blocks := []SlackBlock{{Type: "header", Text: SlackText{Type: "plain_text", Text: header}}}
	if len(links) > 0 {
		var text string
		for _, l := range links {
			line := EscapeMrkdwn(l)
			if next := text + line; utf8.RuneCountInString(next) <= slackSectionMax-1 {
				text = next + "\n"
			}
		}
		if text != "" {
			blocks = append(blocks, mrkdwnBlock(strings.TrimSuffix(text, "\n")))
		}
	}
	if note != "" {
		blocks = append(blocks, mrkdwnBlock(fitEscaped(note, slackSectionMax, cutWithEllipsis)))
	}
	pairs := len(commands)
	if len(blocks)+2*pairs > slackBlocksMax {
		pairs = (slackBlocksMax - len(blocks) - 1) / 2 // one block is kept for the "not shown" line
	}
	for _, c := range commands[:pairs] {
		blocks = append(blocks,
			mrkdwnBlock("*"+fitEscaped(c.Title, slackSectionMax-2, cutWithEllipsis)+"*"),
			mrkdwnBlock(outputBlockText(c.Output, maxChars)))
	}
	if dropped := len(commands) - pairs; dropped > 0 {
		blocks = append(blocks, mrkdwnBlock(fmt.Sprintf("… %d more sections not shown", dropped)))
	}
	return SlackBlocks{Blocks: blocks}
}

// SlackPoster posts messages to a Slack incoming webhook. A nil Client means http.DefaultClient and
// a nil Sleep means a wait that ends early when the context is done. Timeout, when above 0, limits
// each attempt (each request and its reply) on its own.
type SlackPoster struct {
	Client  *http.Client
	Sleep   func(time.Duration)
	Timeout time.Duration
}

// SlackStatusError is Slack answering with a status other than 200. Body is the start of Slack's
// reply. It never contains the webhook URL.
type SlackStatusError struct {
	StatusCode int
	Body       string
}

func (e *SlackStatusError) Error() string {
	return fmt.Sprintf("slack: HTTP %d: %s", e.StatusCode, e.Body)
}

// retryWait reads the Retry-After seconds header: default 2 s, at most 30 s.
func retryWait(h http.Header) time.Duration {
	d := retryDefault
	if n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && n >= 0 {
		d = time.Duration(n) * time.Second
	}
	return min(d, retryCap)
}

func (p *SlackPoster) wait(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		done := make(chan struct{})
		go func() { p.Sleep(d); close(done) }()
		select {
		case <-done:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// postOnce makes one request, limited to p.Timeout when set. retry reports a 429 or 5xx, with the wait
// the server asked for, or an attempt cut by p.Timeout while ctx is still live. Errors never contain
// the webhook URL; a status other than 200 is a *SlackStatusError.
func (p *SlackPoster) postOnce(ctx context.Context, webhook string, payload []byte) (retry bool, wait time.Duration, err error) {
	attemptCtx := ctx
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, webhook, bytes.NewReader(payload))
	if err != nil {
		return false, 0, errors.New("slack: invalid webhook URL")
	}
	req.Header.Set("Content-Type", "application/json")
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // *url.Error carries the URL
		}
		timedOut := attemptCtx.Err() != nil && ctx.Err() == nil
		return timedOut, retryDefault, fmt.Errorf("slack: post failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxReplyRead))
	if resp.StatusCode == http.StatusOK {
		return false, 0, nil
	}
	shown := body
	if len(shown) > maxReplyShown {
		shown = shown[:maxReplyShown]
	}
	err = &SlackStatusError{StatusCode: resp.StatusCode, Body: strings.ToValidUTF8(strings.TrimSpace(string(shown)), "")}
	retry = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	return retry, retryWait(resp.Header), err
}

// Post sends msg to the webhook. Success is HTTP 200. On 429 or 5xx it waits Retry-After and tries
// once more, as it does after an attempt cut by p.Timeout (waiting 2 s); anything else, or a second
// failure, is an error with the status and the start of the reply.
func (p *SlackPoster) Post(ctx context.Context, webhook string, msg SlackBlocks) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("slack: encode message: %w", err)
	}
	retry, wait, err := p.postOnce(ctx, webhook, payload)
	if err == nil || !retry {
		return err
	}
	if werr := p.wait(ctx, wait); werr != nil {
		return fmt.Errorf("%w (gave up waiting to retry: %v)", err, werr)
	}
	_, _, err = p.postOnce(ctx, webhook, payload)
	return err
}
