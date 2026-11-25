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
const RedactionVersion = "redact-v1.2"

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
	// skip, when set, leaves a match alone, such as a long word that is not
	// random enough to be a secret.
	skip func(string) bool
}

// redactRules run in order; earlier, more specific rules win. They are
// deterministic patterns, not a model: they catch common secret formats,
// contact details and names or places in contexts that announce them ("my
// name is", "Mr.", "Author:", "… High School"). Names without such context
// are missed; collector/testdata/redact_eval.json measures how many.
var redactRules = []redactRule{
	{"PRIVATE_KEY", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), 0, nil},
	{"URL_CREDENTIALS", regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^\s/:@]+:[^\s/@]+)@`), 2, nil},
	{"AWS_KEY", regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`), 0, nil},
	{"GITHUB_TOKEN", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`), 0, nil},
	{"SLACK_TOKEN", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`), 0, nil},
	{"API_KEY", regexp.MustCompile(`\b(sk|pk|rk|whsec)[-_](live|test|proj|ant)?[-_]?[A-Za-z0-9_-]{16,}\b`), 0, nil},
	{"JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), 0, nil},
	{"BEARER_TOKEN", regexp.MustCompile(`(?i)\b(bearer|basic)\s+([A-Za-z0-9._~+/=-]{12,})`), 2, nil},
	{"SECRET_ASSIGNMENT", regexp.MustCompile(`(?i)\b([a-z0-9_]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)s?|pass|pwd?)\b(["']?\s*[:=]\s*["']?)([^\s"',;]{4,})`), 3, nil},
	// "the wifi password at school is Tigers2026!"
	{"SECRET_ASSIGNMENT", regexp.MustCompile(`(?i)\b(?:password|passwd|passcode|pin)\b[^.\n]{0,30}?\b(?:is|was)\s+["']?([^\s"',;]{4,})`), 1, nil},
	// Git author lines carry a real name next to the email.
	{"PERSON", regexp.MustCompile(`(?m)^\s*(?:Author|Co-authored-by|Signed-off-by):\s+([^<\n]+?)\s*<`), 1, nil},
	{"EMAIL", regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`), 0, nil},
	{"PHONE", regexp.MustCompile(`(?:\+\d{1,3}[\s.-]?)?(?:\(\d{2,4}\)[\s.-]?|\b\d{2,4}[\s.-])\d{3,4}[\s.-]\d{3,4}\b`), 0, fewDigits},
	{"PHONE", regexp.MustCompile(`\+\d{1,3}(?:[\s.-]?\d{2,6}){2,4}\b`), 0, fewDigits},
	// UK numbers written from the trunk 0: 07911 123456, 07700900461, 020 7946 0958.
	{"PHONE", regexp.MustCompile(`\b0\d{2,4}[\s-]?\d{3,4}[\s-]?\d{3,4}\b`), 0, notUKNumber},
	// UK postcodes: SW1A 1AA, M14 6AB, B33 8TH. The inward letters never use
	// C, I, K, M, O or V. "8GB" and "1TB" are left alone, so "M2 8GB" stays.
	{"ADDRESS", regexp.MustCompile(`\b(?:[A-Z]{1,2}\d[A-Z\d]?|GIR) ?\d[ABD-HJLNP-UW-Z]{2}\b`), 0, func(s string) bool {
		return strings.HasSuffix(s, "GB") || strings.HasSuffix(s, "TB")
	}},
	{"IP_ADDRESS", regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`), 0, func(s string) bool {
		// Loopback and unspecified addresses identify nobody and appear in many error messages.
		return strings.HasPrefix(s, "127.") || s == "0.0.0.0"
	}},
	// Windows account names may contain spaces: C:\Users\Ethan Nguyen\...
	{"HOME_USER", regexp.MustCompile(`[A-Za-z]:\\Users\\([^\\\n"']+)\\`), 1, nil},
	{"HOME_USER", regexp.MustCompile(`(/home/|/Users/|[A-Za-z]:\\Users\\)([^/\\\s"']+)`), 2, nil},
	// Shell prompts such as "(venv) olivia@olivias-macbook ~/code %".
	{"HOME_USER", regexp.MustCompile(`(?m)^(?:\([^)\n]*\)\s*)?([a-z_][a-z0-9_.-]*@[A-Za-z0-9.-]+)[ :]`), 1, nil},
	{"DATE_OF_BIRTH", regexp.MustCompile(`(?i)\b(?:born|dob|date of birth|birthday is)[:\s]+(?:on\s+)?(\d{4}-\d{2}-\d{2}|\d{1,2}[/.-]\d{1,2}[/.-]\d{2,4}|[a-z]+ \d{1,2},? \d{4})`), 1, nil},
	// A street address, with the city, state and postcode that follow it.
	{"ADDRESS", regexp.MustCompile(`\b\d{1,5}\s+(?:[A-Z][a-z]+\s+){1,3}(?:Street|St|Avenue|Ave|Road|Rd|Lane|Ln|Drive|Dr|Terrace|Boulevard|Blvd|Court|Ct|Way|Place|Pl)\b\.?(?:,\s*[A-Z][A-Za-z]+(?:\s+[A-Z][A-Za-z]+)?(?:\s+[A-Z]{2})?(?:\s+\d{5}(?:-\d{4})?)?)?`), 0, nil},
	{"SCHOOL", regexp.MustCompile(`\b(?:St\.?\s+)?(?:[A-Z][A-Za-z'.-]+\s+){1,3}(?:High|Middle|Elementary|Primary|Secondary|Grammar)(?:\s+School)?\b|\b(?:St\.?\s+)?(?:[A-Z][A-Za-z'.-]+\s+){1,3}(?:Academy|School|College)\b`), 0, notName},
	{"SCHOOL", regexp.MustCompile(`(?i)\b(?:go to|goes to|attend|attends|student at|from)\s+([a-z][a-z'.-]+(?:\s+[a-z][a-z'.-]+)?\s+(?:high|middle|elementary|academy|school)(?:\s+school)?)\b`), 1, notName},
	{"PLACE", regexp.MustCompile(`\b(?:[A-Z][A-Za-z'.-]+\s+){1,3}(?:Library|Community Center|Community Centre|Rec Center)\b`), 0, notName},
	{"PERSON", regexp.MustCompile(`\b(?:Mr|Mrs|Ms|Miss|Mx|Dr|Prof)\.?\s+([A-Z][A-Za-z'-]+)`), 1, nil},
	{"PERSON", regexp.MustCompile(`(?i:\bmy name is|\bmy name's)\s+([A-Za-z][A-Za-z'-]+(?:\s+[A-Z][A-Za-z'-]+)?)`), 1, nil},
	{"PERSON", regexp.MustCompile(`(?i:\b(?:hi|hey|hello|yo)[,!]?\s+(?:i'm|im|i am|it's|its|this is))\s+([A-Za-z][a-z'-]+)`), 1, nil},
	{"PERSON", regexp.MustCompile(`(?i:\b(?:i'm|i am|it's|this is))\s+([A-Z][a-z'-]+(?:\s+[A-Z][A-Za-z'-]+)?)`), 1, notName},
	{"PERSON", regexp.MustCompile(`(?i:\b(?:made by|built by|created by|written by|friend|leader is|teacher is|mentor is|coach is))\s+([A-Z][a-z'-]+\s+[A-Z][A-Za-z'-]+)`), 1, notName},
	// @mentions after other text on the line (an indented decorator has only
	// whitespace before it), profile URLs, and "signed in as".
	{"HANDLE", regexp.MustCompile(`[^\s][ \t]+@([A-Za-z][A-Za-z0-9_.]{1,28}[A-Za-z0-9_])`), 1, nil},
	{"HANDLE", regexp.MustCompile(`(?i)\b(?:twitter|x|instagram|github|tiktok|youtube|twitch|reddit|discord)\.com/(?:u/|user/)?(@?[A-Za-z0-9_.-]+)`), 1, nil},
	{"HANDLE", regexp.MustCompile(`(?i)\b(?:signed in as|logged in as|username is|username:|my discord is|my insta is|my github is)\s+@?([A-Za-z0-9_.-]{3,30})`), 1, nil},
	{"HANDLE", regexp.MustCompile(`\b([A-Za-z0-9]+[_.-][A-Za-z0-9_.-]+)\s+on\s+(?:GitHub|Discord|Scratch|Replit|Roblox|Instagram|TikTok)\b`), 1, nil},
	{"HIGH_ENTROPY", regexp.MustCompile(`\b[A-Za-z0-9+/_=-]{32,}\b`), 0, func(s string) bool { return !highEntropy(s) }},
}

func fewDigits(s string) bool { return digits(s) < 9 }

// notUKNumber skips digit runs that are not 10 or 11 digits long, and the
// digit alphabet "0123456789" that appears in code.
func notUKNumber(s string) bool {
	n := digits(s)
	return (n != 10 && n != 11) || strings.HasPrefix(s, "0123456789")
}

// notCapitalisedNames lists words that start capitalised phrases which are
// not names of people or schools, so "Built by Team Nebula" or "It's Python"
// is left alone.
var notCapitalisedNames = map[string]bool{
	"team": true, "the": true, "hack": true, "club": true, "google": true, "microsoft": true,
	"apple": true, "python": true, "java": true, "javascript": true, "react": true, "rust": true,
	"node": true, "linux": true, "windows": true, "not": true, "just": true, "still": true,
	"getting": true, "using": true, "trying": true, "working": true, "new": true, "this": true,
	"public": true, "open": true, "online": true, "code": true, "coding": true, "summer": true,
}

func notName(s string) bool {
	first := strings.ToLower(strings.TrimRight(strings.Fields(s)[0], ".,'"))
	return notCapitalisedNames[first]
}

// Redactor replaces sensitive spans with typed placeholders that carry a
// per-install salted hash, so a reviewer can tell that the same value
// appeared twice without learning it.
type Redactor struct {
	mac   func(parts ...string) string
	terms []string // extra words the user asked to remove, such as their name
	paths []pathAlias
	ner   *NERHelper // optional model; nil uses the rules alone
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

// WithNER adds an entity model that runs after the rules. With it, a model
// failure withholds the item: the user chose the model, so text is never
// sent with only rule-based redaction instead.
func (r *Redactor) WithNER(h *NERHelper) *Redactor {
	r.ner = h
	return r
}

// Version names the redaction applied, including the model when one runs.
func (r *Redactor) Version() string {
	if r.ner != nil {
		return RedactionVersion + "+ner:" + r.ner.Model
	}
	return RedactionVersion
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
			if rule.skip != nil && rule.skip(m) {
				return m
			}
			p := r.placeholder(rule.kind, m)
			found = append(found, Redaction{rule.kind, p})
			return p
		})
	}
	if r.ner != nil && strings.TrimSpace(out) != "" {
		entities, err := r.ner.Entities(out)
		if err != nil {
			return out, found, false
		}
		// Longest first, so "Grace Lee" is replaced before "Grace".
		sort.Slice(entities, func(i, j int) bool { return len(entities[i].Text) > len(entities[j].Text) })
		for _, e := range entities {
			value := strings.TrimSpace(e.Text)
			if len([]rune(value)) < 2 || strings.ContainsAny(value, "[]") {
				continue
			}
			kind := e.Type
			if !entityTypeRe.MatchString(kind) {
				kind = "NAME"
			}
			re := regexp.MustCompile(`(^|[^\pL\pN_])(` + regexp.QuoteMeta(value) + `)($|[^\pL\pN_])`)
			out = replaceGroup(re, out, 2, func(m string) string {
				p := r.placeholder(kind, m)
				found = append(found, Redaction{kind, p})
				return p
			})
		}
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
	// Check each stretch between placeholders on its own, as Redact does;
	// joining them would let context rules match across a placeholder, such
	// as "hi im [PERSON:…] and" reading "and" as a name.
	for _, segment := range placeholderRe.Split(text, -1) {
		for _, rule := range redactRules {
			for _, m := range rule.re.FindAllStringSubmatch(segment, -1) {
				v := m[rule.group]
				if v == "" || (rule.skip != nil && rule.skip(v)) {
					continue
				}
				return false
			}
		}
	}
	return true
}

// entityTypeRe keeps a model's entity type usable inside a placeholder.
var entityTypeRe = regexp.MustCompile(`^[A-Z][A-Z_]{1,19}$`)

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
