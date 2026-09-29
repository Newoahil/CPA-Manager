package ollama

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"golang.org/x/net/html"
)

// parsed is the raw result of a settings-page scrape, before window names are
// finalised.
type parsed struct {
	windows []domain.QuotaWindow
	plan    string
	balance string
}

var (
	rePercent = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*%`)
	reReset   = regexp.MustCompile(`(?i)resets?\s+(?:in|on|at)\s+(.+)`)
	reLabel5h = regexp.MustCompile(`(?i)(rolling|5[\s-]?hour|five[\s-]?hour|hourly)`)
	reLabel7d = regexp.MustCompile(`(?i)(weekly|7[\s-]?day|seven[\s-]?day|week)`)
	reDur     = regexp.MustCompile(`(?i)(\d+)\s*([a-z]+)`)
	rePlanLn  = regexp.MustCompile(`(?i)\bplan\b[:\s]+([A-Za-z][\w -]{0,20})`)
	reMoney   = regexp.MustCompile(`[$€£]\s?\d[\d.,]*`)
	reCredit  = regexp.MustCompile(`(?i)(?:balance|credit)[^\d$€£]{0,12}([$€£]?\s?\d[\d.,]*)`)
)

var knownPlans = []string{"enterprise", "team", "pro", "max", "free", "trial"}

// blockTags are elements whose boundaries should become line breaks when the
// DOM is flattened to text. The set is intentionally broad so the parser keeps
// working across markup changes.
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"br": true, "dd": true, "div": true, "dl": true, "dt": true, "fieldset": true,
	"figcaption": true, "figure": true, "footer": true, "form": true, "h1": true,
	"h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "header": true,
	"hr": true, "li": true, "main": true, "nav": true, "ol": true, "p": true,
	"pre": true, "section": true, "table": true, "td": true, "th": true,
	"tr": true, "ul": true,
}

// parse flattens the settings HTML to text and applies heuristics to recover
// plan, rolling 5h / weekly 7d usage and balance. now anchors relative reset
// text ("Resets in 3h") to an absolute time.
func parse(document string, now time.Time) parsed {
	var out parsed
	if strings.TrimSpace(document) == "" {
		return out
	}
	doc, err := html.Parse(strings.NewReader(document))
	if err != nil {
		return out
	}
	lines := textLines(doc)

	out.plan = extractPlan(lines)
	out.balance = extractBalance(lines)

	used := map[int]bool{}
	for _, spec := range []struct {
		name  string
		label *regexp.Regexp
	}{{"5h", reLabel5h}, {"7d", reLabel7d}} {
		idx := indexLabel(lines, spec.label)
		if idx < 0 {
			continue
		}
		w := domain.QuotaWindow{Name: spec.name}
		if pct, pidx := findPercent(lines, idx, used); pidx >= 0 {
			used[pidx] = true
			if p, ok := parsePercent(pct); ok {
				w.UsedPercent = &p
			}
		}
		if reset, ridx := findReset(lines, idx); reset != "" {
			_ = ridx
			if at, ok := parseReset(reset, now); ok {
				w.ResetAt = &at
			} else {
				w.ResetText = reset
			}
		}
		// Only a numeric percentage makes a window real; a lone reset hint
		// would otherwise masquerade as a successful quota reading.
		if w.UsedPercent == nil {
			continue
		}
		out.windows = append(out.windows, w)
	}
	return out
}

// textLines walks the DOM and returns whitespace-normalised text lines,
// inserting a break at block-element boundaries.
func textLines(n *html.Node) []string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
			return
		}
		if node.Type == html.ElementNode {
			if blockTags[strings.ToLower(node.Data)] {
				b.WriteByte('\n')
			}
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			if blockTags[strings.ToLower(node.Data)] {
				b.WriteByte('\n')
			}
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)

	var out []string
	for _, raw := range strings.Split(b.String(), "\n") {
		if line := strings.Join(strings.Fields(raw), " "); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func indexLabel(lines []string, re *regexp.Regexp) int {
	for i, line := range lines {
		if re.MatchString(line) {
			return i
		}
	}
	return -1
}

// findPercent looks for a percentage on the label line first, then nearby
// lines, skipping indices already claimed by another window.
func findPercent(lines []string, idx int, used map[int]bool) (float64, int) {
	if m := rePercent.FindStringSubmatch(lines[idx]); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			return v, idx
		}
	}
	for _, off := range []int{1, -1, 2, -2, 3, -3} {
		i := idx + off
		if i < 0 || i >= len(lines) || used[i] {
			continue
		}
		m := rePercent.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			return v, i
		}
	}
	return 0, -1
}

func findReset(lines []string, idx int) (string, int) {
	for _, off := range []int{0, 1, 2, -1, 3} {
		i := idx + off
		if i < 0 || i >= len(lines) {
			continue
		}
		if m := reReset.FindStringSubmatch(lines[i]); m != nil {
			if text := strings.TrimSpace(m[1]); text != "" {
				return text, i
			}
		}
	}
	return "", -1
}

// parseReset turns a reset hint into an absolute time when possible. Absolute
// RFC3339/date strings and relative "3h"/"6 days" text are both accepted; an
// unrecognised string yields ok=false so it is kept verbatim as ResetText.
func parseReset(text string, now time.Time) (time.Time, bool) {
	s := strings.TrimSpace(text)
	if s == "" {
		return time.Time{}, false
	}
	if d, ok := parseDurationText(s); ok {
		return now.Add(d), true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseDurationText sums "3h", "6 days", "2 hours 30 minutes" style text that
// follows a "Resets in" prefix.
func parseDurationText(s string) (time.Duration, bool) {
	matches := reDur.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return 0, false
	}
	var total time.Duration
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		// Units arrive singular, plural or abbreviated ("6 days", "3h", "90m"),
		// so fold the plural 's' away before switching.
		unit := strings.TrimSuffix(strings.ToLower(m[2]), "s")
		switch unit {
		case "d", "day":
			total += time.Duration(n) * 24 * time.Hour
		case "h", "hr", "hour":
			total += time.Duration(n) * time.Hour
		case "m", "min", "minute":
			total += time.Duration(n) * time.Minute
		case "w", "week":
			total += time.Duration(n) * 7 * 24 * time.Hour
		case "mo", "month":
			total += time.Duration(n) * 30 * 24 * time.Hour
		}
	}
	if total <= 0 {
		return 0, false
	}
	return total, true
}

func parsePercent(v float64) (float64, bool) {
	if v < 0 {
		return 0, false
	}
	if v > 100 {
		return 100, true
	}
	return v, true
}

func extractPlan(lines []string) string {
	for _, line := range lines {
		if m := rePlanLn.FindStringSubmatch(line); m != nil {
			if p := strings.TrimSpace(m[1]); p != "" {
				return p
			}
		}
	}
	for _, line := range lines {
		lower := strings.ToLower(line)
		for _, plan := range knownPlans {
			if containsWord(lower, plan) {
				return strings.ToUpper(plan[:1]) + plan[1:]
			}
		}
	}
	return ""
}

// containsWord avoids matching "pro" inside "profile" and "free" inside
// "freely" by checking non-letter boundaries.
func containsWord(s, word string) bool {
	at := strings.Index(s, word)
	if at < 0 {
		return false
	}
	before := at == 0 || !isLetter(rune(s[at-1]))
	after := at+len(word) == len(s) || !isLetter(rune(s[at+len(word)]))
	return before && after
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func extractBalance(lines []string) string {
	for _, line := range lines {
		if !strings.Contains(strings.ToLower(line), "balance") && !strings.Contains(strings.ToLower(line), "credit") {
			continue
		}
		if m := reCredit.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1])
		}
		if m := reMoney.FindString(line); m != "" {
			return m
		}
	}
	// Fall back to a bare currency amount when it is the only money-like line.
	for _, line := range lines {
		if m := reMoney.FindString(line); m != "" {
			return m
		}
	}
	return ""
}
