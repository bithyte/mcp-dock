package oauth

import (
	"bytes"
	"html/template"
	"net/http"
)

// Pages use html/template so the client name (attacker-controlled, set at
// registration) and every other field are HTML-escaped.

const pageStyle = `<style>
body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#222;line-height:1.5}
h1{font-size:1.4rem}
.box{border:1px solid #ccc;border-radius:6px;padding:1rem;margin:1rem 0;overflow-wrap:anywhere}
button{font-size:1rem;padding:.5rem 1.2rem;margin-right:.5rem;cursor:pointer}
</style>`

var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorize {{.ClientName}}</title>` + pageStyle + `</head>
<body>
<h1>Authorize access to mcp-dock</h1>
<p>Signed in with GitHub as <strong>{{.Login}}</strong>.</p>
<div class="box">
<p>The application <strong>{{.ClientName}}</strong> wants to use the tools on this gateway as you.</p>
<p>If you approve, you'll be sent back to <strong>{{.RedirectHost}}</strong>.</p>
</div>
<p>Only approve if you started this from an app you trust and recognize that address.</p>
<form method="post" action="/oauth/consent">
<input type="hidden" name="request" value="{{.RequestID}}">
<button type="submit" name="action" value="approve">Approve</button>
<button type="submit" name="action" value="deny">Deny</button>
</form>
</body></html>
`))

var errorTmpl = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>` + pageStyle + `</head>
<body>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
</body></html>
`))

type consentData struct {
	ClientName, RedirectHost, Login, RequestID string
}

type errorData struct {
	Title, Message string
}

// writeHTML renders t with the headers every HTML response carries. The page
// can't be framed (clickjacking the Approve button) or cached.
func (s *Server) writeHTML(w http.ResponseWriter, status int, t *template.Template, data any) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		s.log.Error("oauth page render failed", "page", t.Name(), "err", err)
		status = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString("internal error\n")
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// errorPage writes an HTML error page. Nothing is redirected.
func (s *Server) errorPage(w http.ResponseWriter, status int, title, msg string) {
	s.writeHTML(w, status, errorTmpl, errorData{Title: title, Message: msg})
}
