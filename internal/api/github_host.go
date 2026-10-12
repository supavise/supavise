package api

import (
	"context"
	"errors"
	"sort"

	"github.com/supavise/supavise/internal/github"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/projectconfig"
)

// ghHost is what the GitHub integration uses of this API: it saves settings and deploys
// functions the way the routes of the same names do (github.Host).
type ghHost struct{ s *Server }

// DeployFunction stores the sources of a function of the repository, bundled where the node bundles.
func (h ghHost) DeployFunction(ctx context.Context, ref string, f github.FunctionDeploy) error {
	files := make([]FunctionFile, len(f.Files))
	for i, fl := range f.Files {
		files[i] = FunctionFile{Path: fl.Path, Content: fl.Content}
	}
	_, err := h.s.storeSources(ctx, ref, sourceUpload{Slug: f.Slug, Name: f.Slug, VerifyJWT: f.VerifyJWT, Entrypoint: f.Entrypoint,
		ImportMap: f.ImportMap, Static: f.StaticPatterns, Files: files})
	return err
}

// FunctionSecret reads one Edge Function secret of the project: what env(NAME) of config.toml means.
func (h ghHost) FunctionSecret(ctx context.Context, ref, name string) (string, bool, error) {
	secs, err := h.s.store.ListFunctionSecrets(ctx, ref)
	if err != nil {
		return "", false, err
	}
	for _, sec := range secs {
		if sec.Name != name {
			continue
		}
		b, err := h.s.sec.Open(sec.Sealed)
		if err != nil {
			return "", false, err
		}
		return string(b), true, nil
	}
	return "", false, nil
}

// SaveSettings saves and applies settings of one service, as the PATCH of its config does. A
// validation failure comes back as the message of the refusal.
func (h ghHost) SaveSettings(ctx context.Context, ref string, svc projectconfig.Service, patch map[string]any) ([]string, error) {
	p, err := h.s.loadProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	var ch *projectconfig.Change
	if svc == projectconfig.PostgREST {
		// The route's own refusal of a schema that does not exist: without it the restart would wait out
		// PostgREST's readiness timeout and roll the save back.
		err = h.s.checkExposedSchemas(ctx, p, patch)
	}
	if err == nil {
		ch, _, err = h.s.saveSettings(ctx, p, svc, patch, lifecycle.ApplyOptions{})
	}
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) {
			return nil, errors.New(ae.Message)
		}
		return nil, err
	}
	changed := append([]string(nil), ch.Changed...)
	sort.Strings(changed)
	return changed, nil
}
