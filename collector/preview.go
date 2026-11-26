// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"crypto/subtle"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// servePreview serves the submission preview on 127.0.0.1 only. The page
// shows the redacted bundle exactly as it would be uploaded; the raw JSON
// link returns the same bytes, and Submit uploads the bytes whose digest the
// page showed, never a rebuilt bundle. A random token in the URL keeps other
// local web pages from reading or submitting it.
func servePreview(store *Store, base BuildOptions, apiURL, apiKey string) error {
	token := randomHex(16)
	var mu sync.Mutex
	built := map[string][]byte{}

	build := func(r *http.Request) (*Bundle, []byte, string, error) {
		opt := base
		opt.Now = time.Now()
		opt.Exclude = map[string]bool{}
		for k := range base.Exclude {
			opt.Exclude[k] = true
		}
		for _, id := range r.Form["exclude"] {
			opt.Exclude[id] = true
		}
		if r.Form.Has("context") {
			opt.UserContext = r.FormValue("context")
			// The box is in the same form, so an unticked box means no.
			opt.AllowModelRelabel = r.FormValue("allow_model_relabel") == "1"
		}
		if v := r.FormValue("redact"); v != "" {
			opt.Terms = append(append([]string{}, base.Terms...), strings.Split(v, ",")...)
		}
		b, err := store.BuildBundle(opt)
		if err != nil {
			return nil, nil, "", err
		}
		data, digest := b.Encode()
		mu.Lock()
		built[digest] = data
		mu.Unlock()
		return b, data, digest, nil
	}

	authorised := func(r *http.Request) bool {
		return subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(token)) == 1
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !authorised(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = r.ParseForm()
		if !r.Form.Has("context") {
			r.Form["context"] = []string{base.UserContext}
			if base.AllowModelRelabel {
				r.Form["allow_model_relabel"] = []string{"1"}
			}
		}
		b, data, digest, err := build(r)
		page := previewPage{Token: token, Error: errString(err), Form: r.Form}
		if err == nil {
			page.Bundle, page.Digest, page.Size, page.Raw = b, digest, len(data), string(data)
			page.Devlog = DraftDevlog(b, time.Local)
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
		_ = previewTemplate.Execute(w, page)
	})
	mux.HandleFunc("/bundle.json", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		data, ok := built[r.URL.Query().Get("digest")]
		mu.Unlock()
		if !authorised(r) || !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !authorised(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		digest := r.FormValue("digest")
		mu.Lock()
		data, ok := built[digest]
		mu.Unlock()
		if !ok {
			http.Error(w, "unknown bundle; reload the preview", http.StatusConflict)
			return
		}
		if apiURL == "" || apiKey == "" {
			http.Error(w, "set api_url and api_key in ~/.wakatime.cfg to submit", http.StatusPreconditionFailed)
			return
		}
		status, out, err := Upload(apiURL, apiKey, data, digest)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch {
		case err != nil:
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "Upload failed, nothing was recorded as submitted: %v\nRetrying sends the same bytes and is safe.\n", err)
		case status >= 300:
			w.WriteHeader(status)
			fmt.Fprintf(w, "The server refused the submission (%d):\n%s\n", status, out)
		default:
			fmt.Fprintf(w, "Submitted %d bytes, sha256 %s.\nServer response:\n%s\n", len(data), digest, out)
			log.Printf("submitted bundle %s", digest)
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	fmt.Printf("Preview (this device only): http://%s/?t=%s\n", ln.Addr(), token)
	return http.Serve(ln, mux)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type previewPage struct {
	Token, Digest, Raw, Error string
	Size                      int
	Bundle                    *Bundle
	Devlog                    string
	Form                      map[string][]string
}

var previewTemplate = template.Must(template.New("preview").Funcs(template.FuncMap{
	"hours": func(s int64) string { return fmt.Sprintf("%.2fh", float64(s)/3600) },
	"when": func(ms int64) string {
		return time.UnixMilli(ms).Local().Format("2006-01-02 15:04:05")
	},
	"excluded": func(form map[string][]string, id string) bool {
		for _, v := range form["exclude"] {
			if v == id {
				return true
			}
		}
		return false
	},
	"first": func(form map[string][]string, key string) string {
		if v := form[key]; len(v) > 0 {
			return v[0]
		}
		return ""
	},
}).Parse(`<!doctype html><html><head><meta charset="utf-8"><title>strgl submission preview</title>
<style>
body{font:14px/1.45 system-ui,sans-serif;margin:24px auto;max-width:1100px;padding:0 16px;color:#1a1a1a}
h1{font-size:20px}h2{font-size:16px;margin-top:28px}
.turn{border:1px solid #ccc;border-radius:6px;padding:12px;margin:12px 0}
.l1{border-left:6px solid #2f855a}.l0{border-left:6px solid #a0aec0}
.badge{display:inline-block;padding:1px 7px;border-radius:10px;background:#edf2f7;font-size:12px;margin-right:4px}
.review{background:#fefcbf}.item{margin:8px 0;padding:8px;background:#f7fafc;border-radius:4px}
.withheld,.purged,.excluded{background:#fff5f5;color:#9b2c2c}
pre{white-space:pre-wrap;word-break:break-word;margin:4px 0;font-size:12px}
.totals td{padding:2px 12px 2px 0}.err{color:#9b2c2c;font-weight:bold}
textarea,input[type=text]{width:100%;box-sizing:border-box}
</style></head><body>
<h1>Submission preview</h1>
<p>This page runs on your device. Everything below is the redacted bundle that <b>Submit</b> sends, byte for byte. Raw prompts, outputs and diffs stay in your local evidence store.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{with .Bundle}}
<p>{{if .ModelRelabelConsent}}You allow a model re-check of the labels.{{else}}No model re-check: reviewers only.{{end}}</p>
<p>Project <b>{{.Project}}</b>, {{when .From}} to {{when .To}}. Rule set {{.RuleSet}}, redaction {{.RedactionVersion}}, key protection {{.KeyProtection}}.</p>
<table class="totals">
<tr><td>Estimated human activity (2-minute rule)</td><td><b>{{hours .Totals.EstimatedHumanSeconds}}</b></td></tr>
<tr><td>Agent running, not credited</td><td>{{hours .Totals.AgentSeconds}}</td></tr>
<tr><td>Proposed productive time (turns labelled 1)</td><td><b>{{hours .Totals.ProposedProductiveSeconds}}</b></td></tr>
<tr><td>Turns</td><td>{{.Totals.Turns}}: {{.Totals.LabelledOne}} labelled 1, {{.Totals.NeedsReview}} need review, {{.Totals.Backfilled}} backfilled</td></tr>
<tr><td>Items withheld by redaction / excluded by you</td><td>{{.Totals.WithheldItems}} / {{.Totals.ExcludedItems}}</td></tr>
</table>
<p>A reviewer decides the approved hours. Labels are proposals; you can add context but not change them.</p>
{{end}}
<form method="get" action="/">
<input type="hidden" name="t" value="{{.Token}}">
<h2>Your context for the reviewer</h2>
<textarea name="context" rows="3">{{first .Form "context"}}</textarea>
<p><label><input type="checkbox" name="allow_model_relabel" value="1" {{if eq (first .Form "allow_model_relabel") "1"}}checked{{end}}> A reviewer may send this redacted bundle (nothing else) to an external AI model to re-check the labels. It never changes your hours by itself, and escalated cases are never sent.</label></p>
<p>Extra words to redact (comma-separated, such as your name or school): <input type="text" name="redact" value="{{first .Form "redact"}}"></p>
{{with .Bundle}}{{range .Turns}}
<div class="turn l{{.Label.Value}}">
<div><b>{{when .StartedAt}}</b> {{.Harness}} {{.Model}}
<span class="badge">label {{.Label.Value}}</span>{{if .Label.NeedsReview}}<span class="badge review">needs review</span>{{end}}
<span class="badge">coverage {{.Label.Coverage}}</span><span class="badge">{{.EndReason}}</span>{{if .Backfilled}}<span class="badge review">backfilled</span>{{end}}
<span class="badge">{{.Label.ProductiveSeconds}}s proposed</span></div>
<div>{{.Label.Reason}}</div>
{{if .Label.Coaching}}<div><i>Tip: {{.Label.Coaching}}</i></div>{{end}}
<div>Signals: {{range .Label.Signals}}<span class="badge">{{.}}</span>{{end}}</div>
<div>{{.ToolCalls}} tool calls, {{.ChildSessions}} child sessions, {{.InputTokens}} in / {{.OutputTokens}} out tokens{{if .FilesChanged}}, files changed: {{range .FilesChanged}}{{.}} {{end}}{{end}}</div>
{{range .Items}}<div class="item {{.Status}}">
<label><input type="checkbox" name="exclude" value="{{.ID}}" {{if excluded $.Form .ID}}checked{{end}} {{if eq .Status "purged" "withheld"}}disabled{{end}}> leave out</label>
<b>{{.Kind}}</b> {{.Status}} {{if .Tool}}tool {{.Tool}}{{end}} {{if .Command}}<code>{{.Command}}</code>{{end}} {{if .Exit}}exit {{.Exit}}{{end}}{{if .IsError}} (error){{end}}
{{if .Redactions}}<span class="badge">{{len .Redactions}} redacted</span>{{end}}
{{if .Text}}<pre>{{.Text}}</pre>{{end}}
</div>{{end}}
</div>{{end}}{{end}}
<p><button type="submit">Update preview</button></p>
</form>
{{if .Bundle}}
<h2>Exact upload</h2>
<p>{{.Size}} bytes, sha256 <code>{{.Digest}}</code>. <a href="/bundle.json?t={{.Token}}&amp;digest={{.Digest}}">View the raw JSON</a>.</p>
<form method="post" action="/submit?t={{.Token}}"><input type="hidden" name="digest" value="{{.Digest}}"><button type="submit">Submit these exact bytes</button></form>
<details><summary>Raw JSON</summary><pre>{{.Raw}}</pre></details>
<h2>Devlog draft</h2>
<p>Written from the redacted items above, for you to edit and post to your program yourself. It is not part of the upload and is never sent by the collector. Check it for names the redaction missed.</p>
<textarea rows="16" aria-label="Devlog draft">{{.Devlog}}</textarea>
{{end}}
</body></html>`))
