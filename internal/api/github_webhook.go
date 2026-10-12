package api

import (
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/supavise/supavise/internal/github"
)

// ghHookSlots bounds the deliveries whose body is being read: a body is held in memory until the
// signature can be checked, and the route has no credentials.
var ghHookSlots = make(chan struct{}, 16)

var (
	ghHookSlotWait = 2 * time.Second  // how long a delivery waits for a slot
	ghHookReadTime = 15 * time.Second // how long a body may take to arrive
)

// ghWebhook is POST /hooks/github, the URL of the GitHub App's webhook. It is outside /platform,
// /v1 and /v2 and takes no credentials: the signature of the body is the credential, and the
// service checks it before it believes anything (internal/github, webhook.go). Before the body is
// read, the route refuses a declared length over the limit and a request whose signature header
// cannot be GitHub's; it reads at most 16 bodies at a time, each within 15 seconds and the limit.
func (s *Server) ghWebhook(w http.ResponseWriter, r *http.Request) error {
	if s.gh == nil {
		return errf(http.StatusNotFound, "Not Found")
	}
	if r.ContentLength > github.MaxWebhookBody {
		return errf(http.StatusRequestEntityTooLarge, "The delivery is larger than %d bytes", github.MaxWebhookBody)
	}
	if !github.SignatureShapeOK(r.Header.Get("X-Hub-Signature-256")) {
		return errf(http.StatusUnauthorized, "the signature does not match")
	}
	body, err := s.readHookBody(w, r)
	if err != nil {
		return err
	}
	res := s.gh.HandleWebhook(r.Context(), github.Delivery{
		Event: r.Header.Get("X-GitHub-Event"), ID: r.Header.Get("X-GitHub-Delivery"),
		Signature: r.Header.Get("X-Hub-Signature-256"), Body: body,
	})
	writeJSON(w, res.Status, map[string]string{"message": res.Message})
	return nil
}

// readHookBody reads the body of a delivery under one of the route's slots.
func (s *Server) readHookBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	wait := time.NewTimer(ghHookSlotWait)
	defer wait.Stop()
	select {
	case ghHookSlots <- struct{}{}:
		defer func() { <-ghHookSlots }()
	case <-wait.C:
		w.Header().Set("Retry-After", "5")
		return nil, errf(http.StatusServiceUnavailable, "Too many deliveries are arriving at once")
	case <-r.Context().Done():
		return nil, errf(http.StatusServiceUnavailable, "The request ended")
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(ghHookReadTime))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, github.MaxWebhookBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, errf(http.StatusRequestEntityTooLarge, "The delivery is larger than %d bytes", github.MaxWebhookBody)
		}
		return nil, errf(http.StatusBadRequest, "Could not read the delivery")
	}
	return body, nil
}

// ---- the operator's side: creating the server's GitHub App ---------------------------------------

func (s *Server) ghAppStatus(w http.ResponseWriter, r *http.Request) error {
	if s.gh == nil {
		return errf(http.StatusNotFound, "%s", ghNotConfiguredMsg)
	}
	st, err := s.gh.Status(r.Context())
	if err != nil {
		return s.ghErr(r, err)
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

var manifestTmpl = template.Must(template.New("manifest").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Create the GitHub App</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem}button{font:inherit;padding:.5rem 1rem}code{background:#f2f2f2;padding:.1rem .3rem}</style></head>
<body><h1>Create the GitHub App</h1>
<p>Supavise will ask GitHub to create an App that belongs to your account with the permissions it needs to build preview branches from your repositories: read access to contents and metadata, and write access to pull requests, checks and issues (for the comment on a pull request).</p>
<form method="post" action="{{.Action}}"><input type="hidden" name="manifest" value="{{.Manifest}}"><button type="submit">Continue to GitHub</button></form>
<p>GitHub asks you to confirm the App's name, then sends you back here.</p></body></html>`))

var resultTmpl = template.Must(template.New("result").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem}code{background:#f2f2f2;padding:.1rem .3rem}</style></head>
<body><h1>{{.Title}}</h1>{{range .Lines}}<p>{{.}}</p>{{end}}{{if .Link}}<p><a href="{{.Link}}">{{.LinkText}}</a></p>{{end}}</body></html>`))

type resultPage struct {
	Title    string
	Lines    []string
	Link     string
	LinkText string
}

// writePage answers an HTML page that no script, frame or outside request can touch.
func writePage(w http.ResponseWriter, status int, tmpl *template.Template, data any, formAction string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	csp := "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'"
	if formAction != "" {
		csp += "; form-action " + formAction
	}
	h.Set("Content-Security-Policy", csp)
	w.WriteHeader(status)
	_ = tmpl.Execute(w, data)
}

func (s *Server) ghManifestPage(w http.ResponseWriter, r *http.Request) error {
	if s.gh == nil {
		return errf(http.StatusNotFound, "Not Found")
	}
	action, manifest, err := s.gh.ManifestForm(r.Context(), r.URL.Query().Get("state"))
	if errors.Is(err, github.ErrNotFound) {
		writePage(w, http.StatusNotFound, resultTmpl, resultPage{Title: "This link has expired",
			Lines: []string{"The link to create the GitHub App is good once and for half an hour. Run `supavise github app create` again for a new one."}}, "")
		return nil
	}
	if err != nil {
		return s.ghErr(r, err)
	}
	// GitHub's origin is the only place the form may go.
	origin := ""
	if u, perr := url.Parse(action); perr == nil {
		origin = u.Scheme + "://" + u.Host
	}
	writePage(w, http.StatusOK, manifestTmpl, map[string]any{"Action": action, "Manifest": string(manifest)}, origin)
	return nil
}

func (s *Server) ghManifestCallback(w http.ResponseWriter, r *http.Request) error {
	if s.gh == nil {
		return errf(http.StatusNotFound, "Not Found")
	}
	q := r.URL.Query()
	app, err := s.gh.CompleteManifest(r.Context(), q.Get("code"), q.Get("state"))
	switch {
	case errors.Is(err, github.ErrNotFound):
		writePage(w, http.StatusBadRequest, resultTmpl, resultPage{Title: "This link has expired",
			Lines: []string{"The App was not created: the link is used up or too old. Run `supavise github app create` again."}}, "")
		return nil
	case errors.Is(err, github.ErrConflict), errors.Is(err, github.ErrInvalid):
		writePage(w, http.StatusBadRequest, resultTmpl, resultPage{Title: "The App was not created", Lines: []string{trimGH(err)}}, "")
		return nil
	case err != nil:
		s.log.Warn("the GitHub App could not be created", "err", err)
		writePage(w, http.StatusBadGateway, resultTmpl, resultPage{Title: "The App was not created", Lines: []string{"GitHub or this server failed to finish: see the server's log."}}, "")
		return nil
	}
	writePage(w, http.StatusOK, resultTmpl, resultPage{Title: "The GitHub App is created",
		Lines: []string{"The App " + app.Name + " belongs to " + app.OwnerLogin + ". Install it on the account or organization whose repositories you want to connect, then connect a repository in the project's Settings > Integrations > GitHub."},
		Link:  app.InstallURL(s.cfg.GitHub.Web()), LinkText: "Install the App on GitHub"}, "")
	return nil
}
