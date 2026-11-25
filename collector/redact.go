// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// RedactionVersion names the rule list below. It is recorded in every bundle.
const RedactionVersion = "redact-v1.0"

// Redaction is one replaced span, reported without its original text.
type Redaction struct {
	Type        string `json:"type"`
	Placeholder string `json:"placeholder"`
}

type redactRule struct {
	kind string
	re   *regexp.Regexp
	// group is the submatch to replace; 0 replaces the whole match.
	group int
}

// redactRules run in order; earlier, more specific rules win. They are
// deterministic patterns, not a model: they catch common secret formats and
// contact details, not every name or free-text identifier.
var redactRules = []redactRule{
	{"PRIVATE_KEY", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), 0},
	{"URL_CREDENTIALS", regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^\s/:@]+:[^\s/@]+)@`), 2},
	{"AWS_KEY", regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`), 0},
	{"GITHUB_TOKEN", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`), 0},
	{"SLACK_TOKEN", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`), 0},
	{"API_KEY", regexp.MustCompile(`\b(sk|pk|rk)[-_](live|test|proj|ant)?[-_]?[A-Za-z0-9_-]{16,}\b`), 0},
	{"JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), 0},
	{"BEARER_TOKEN", regexp.MustCompile(`(?i)\b(bearer|basic)\s+([A-Za-z0-9._~+/=-]{12,})`), 2},
	{"SECRET_ASSIGNMENT", regexp.MustCompile(`(?i)\b([a-z0-9_]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)s?)\b(["']?\s*[:=]\s*["']?)([^\s"',;]{4,})`), 3},
	{"EMAIL", regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`), 0},
	{"PHONE", regexp.MustCompile(`(?:\+\d{1,3}[\s.-]?)?(?:\(\d{2,4}\)[\s.-]?|\b\d{2,4}[\s.-])\d{3,4}[\s.-]\d{3,4}\b`), 0},
	{"IP_ADDRESS", regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`), 0},
	{"HOME_USER", regexp.MustCompile(`(/home/|/Users/|[A-Za-z]:\\Users\\)([^/\\\s"']+)`), 2},
	{"HIGH_ENTROPY", regexp.MustCompile(`\b[A-Za-z0-9+/_=-]{32,}\b`), 0},
}

// Redactor replaces sensitive spans with typed placeholders that carry a
// per-install salted hash, so a reviewer can tell that the same value
// appeared twice without learning it.
type Redactor struct {
	mac   func(parts ...string) string
	terms []string // extra words the user asked to remove, such as their name
	paths []pathAlias
}

// pathAlias replaces a local directory, such as a project root or the home
// directory, with a fixed name, so absolute paths do not reveal the layout
// of the device.
type pathAlias struct{ path, alias string }

// WithPaths adds directories to replace, longest first.
func (r *Redactor) WithPaths(paths map[string]string) *Redactor {
	for p, alias := range paths {
		if len(p) > 1 {
			r.paths = append(r.paths, pathAlias{strings.TrimRight(p, "/\\"), alias})
		}
	}
	sort.Slice(r.paths, func(i, j int) bool { return len(r.paths[i].path) > len(r.paths[j].path) })
	return r
}

func NewRedactor(store *Store, terms []string) *Redactor {
	var clean []string
	for _, t := range terms {
		if t = strings.TrimSpace(t); len(t) >= 2 {
			clean = append(clean, t)
		}
	}
	sort.Slice(clean, func(i, j int) bool { return len(clean[i]) > len(clean[j]) })
	return &Redactor{mac: store.mac, terms: clean}
}

func (r *Redactor) placeholder(kind, value string) string {
	return fmt.Sprintf("[%s:%s]", kind, r.mac("redact", kind, value)[:8])
}

// Redact returns the redacted text and what was removed. ok is false when a
// verification pass still finds a sensitive pattern; the caller must then
// withhold the text rather than upload it.
func (r *Redactor) Redact(text string) (out string, found []Redaction, ok bool) {
	out = text
	for _, p := range r.paths {
		out = strings.ReplaceAll(out, p.path, p.alias)
	}
	for _, term := range r.terms {
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(term))
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			p := r.placeholder("USER_TERM", strings.ToLower(m))
			found = append(found, Redaction{"USER_TERM", p})
			return p
		})
	}
	for _, rule := range redactRules {
		out = replaceGroup(rule.re, out, rule.group, func(m string) string {
			if rule.kind == "HIGH_ENTROPY" && !highEntropy(m) {
				return m
			}
			if rule.kind == "PHONE" && digits(m) < 9 {
				return m
			}
			p := r.placeholder(rule.kind, m)
			found = append(found, Redaction{rule.kind, p})
			return p
		})
	}
	return out, found, r.clean(out)
}

// clean is the fail-closed check: no rule may still match outside a
// placeholder.
func (r *Redactor) clean(text string) bool {
	stripped := placeholderRe.ReplaceAllString(text, " ")
	for _, p := range r.paths {
		if strings.Contains(stripped, p.path) {
			return false
		}
	}
	for _, term := range r.terms {
		if strings.Contains(strings.ToLower(stripped), strings.ToLower(term)) {
			return false
		}
	}
	for _, rule := range redactRules {
		for _, m := range rule.re.FindAllStringSubmatch(stripped, -1) {
			v := m[rule.group]
			if v == "" || (rule.kind == "HIGH_ENTROPY" && !highEntropy(v)) || (rule.kind == "PHONE" && digits(v) < 9) {
				continue
			}
			return false
		}
	}
	return true
}

var placeholderRe = regexp.MustCompile(`\[[A-Z_]+:[0-9a-f]{8}\]`)

// replaceGroup replaces matches outside existing placeholders, so one rule
// never rewrites another rule's placeholder.
func replaceGroup(re *regexp.Regexp, s string, group int, fn func(string) string) string {
	var b strings.Builder
	last := 0
	for _, idx := range placeholderRe.FindAllStringIndex(s, -1) {
		b.WriteString(replaceSegment(re, s[last:idx[0]], group, fn))
		b.WriteString(s[idx[0]:idx[1]])
		last = idx[1]
	}
	b.WriteString(replaceSegment(re, s[last:], group, fn))
	return b.String()
}

func replaceSegment(re *regexp.Regexp, s string, group int, fn func(string) string) string {
	if group == 0 {
		return re.ReplaceAllStringFunc(s, fn)
	}
	var b strings.Builder
	last := 0
	for _, idx := range re.FindAllStringSubmatchIndex(s, -1) {
		start, end := idx[2*group], idx[2*group+1]
		if start < 0 {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(fn(s[start:end]))
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func digits(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n++
		}
	}
	return n
}

// highEntropy accepts long strings that mix character classes and have
// high Shannon entropy, which excludes most identifiers and words.
func highEntropy(s string) bool {
	var lower, upper, digit bool
	counts := map[rune]float64{}
	for _, c := range s {
		counts[c]++
		switch {
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= '0' && c <= '9':
			digit = true
		}
	}
	classes := 0
	for _, b := range []bool{lower, upper, digit} {
		if b {
			classes++
		}
	}
	if classes < 2 || !digit {
		return false
	}
	entropy := 0.0
	for _, n := range counts {
		p := n / float64(len(s))
		entropy -= p * math.Log2(p)
	}
	return entropy >= 3.5
}
