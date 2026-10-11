package api

import (
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"

	"github.com/supavise/supavise/internal/github"
)

// ghWebhook is POST /hooks/github, the URL of the GitHub App's webhook. It is outside /platform,
// /v1 and /v2 and takes no credentials: the signature of the body is the credential, and the
// service checks it before it believes anything (internal/github, webhook.go). The size and the
// rate are limited before and in the service.
func (s *Server) ghWebhook(w http.ResponseWriter, r *http.Request) error {
	if s.gh == nil {
		return errf(http.StatusNotFound, "Not Found")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, github.MaxWebhookBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errf(http.StatusRequestEntityTooLarge, "The delivery is larger than %d bytes", github.MaxWebhookBody)
		}
		return errf(http.StatusBadRequest, "Could not read the delivery")
	}
	res := s.gh.HandleWebhook(r.Context(), github.Delivery{
		Event: r.Header.Get("X-GitHub-Event"), ID: r.Header.Get("X-GitHub-Delivery"),
		Signature: r.Header.Get("X-Hub-Signature-256"), Body: body,
	})
	writeJSON(w, res.Status, map[string]string{"message": res.Message})
	return nil
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

// ghAppBegin starts GitHub's manifest flow and answers the link the operator opens in a browser
// that is signed in to GitHub.
func (s *Server) ghAppBegin(w http.ResponseWriter, r *http.Request) error {
	if s.gh == nil {
		return errf(http.StatusNotFound, "%s", ghNotConfiguredMsg)
	}
	var in struct {
		Owner   string `json:"owner"`
		Name    string `json:"name"`
		Replace bool   `json:"replace"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	link, err := s.gh.BeginManifest(r.Context(), github.ManifestRequest{Owner: in.Owner, Name: in.Name, Replace: in.Replace,
		CreatedBy: principalFrom(r.Context()).UserID})
	if err != nil {
		return s.ghErr(r, err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": link})
	return nil
}

func (s *Server) ghAppRemove(w http.ResponseWriter, r *http.Request) error {
	if s.gh == nil {
		return errf(http.StatusNotFound, "%s", ghNotConfiguredMsg)
	}
	had, err := s.gh.RemoveApp(r.Context())
	if err != nil {
		return s.ghErr(r, err)
	}
	if !had {
		return errf(http.StatusNotFound, "This server has no GitHub App to remove.")
	}
	writeNoContent(w)
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
