package branching

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// Expiry warning. A branch with a lifetime ([branching] default_ttl, or --ttl) is deleted when it ends,
// and a branch with a custom or vanity hostname loses more than its data: the hostname, its DNS
// records' target and its certificate go with it. When such a branch is within [branching]
// expiry_warning_hours of its end, the sweeper records branch.expiring on the branch and on its
// parent and posts to the branch's notify_url, once per expiry. A branch with no hostname, a
// persistent branch and a branch without a lifetime are not warned.

// hostnames lists the custom and vanity hostnames that are live on ref.
func (s *Service) hostnames(ctx context.Context, ref string) []string {
	store := registry.Domains(s.reg)
	if store == nil {
		return nil
	}
	var out []string
	if h, err := store.GetCustomHostname(ctx, ref); err == nil && h.Status == registry.HostnameActive {
		out = append(out, h.Hostname)
	}
	if v, err := store.GetVanitySubdomain(ctx, ref); err == nil {
		out = append(out, s.cfg.VanityHost(v.Name))
	}
	return out
}

// warnExpiring sends the warning for b if it is due, and reports whether it did.
func (s *Service) warnExpiring(ctx context.Context, b *Branch, now time.Time) bool {
	if b.Persistent || b.ExpiresAt == nil {
		return false
	}
	left := b.ExpiresAt.Sub(now)
	if left <= 0 || left > s.cfg.Branching.ExpiryWarning() {
		return false
	}
	st := registry.BranchLifecycleOf(s.reg)
	if st == nil {
		return false
	}
	if w, err := st.GetExpiryWarning(ctx, b.Ref); err == nil && w.For.Equal(*b.ExpiresAt) {
		return false // this expiry has been warned about
	} else if err != nil && !errors.Is(err, registry.ErrNotFound) {
		return false
	}
	names := s.hostnames(ctx, b.Ref)
	if len(names) == 0 {
		return false
	}
	// Recorded first: a warning that fails to send is lost, but one that cannot be recorded is not
	// repeated every minute.
	if err := st.SetExpiryWarning(ctx, b.Ref, *b.ExpiresAt, now); err != nil {
		s.log.Warn("could not record an expiry warning", "ref", b.Ref, "err", err)
		return false
	}
	payload := map[string]any{
		"branch": b.Ref, "name": b.Name, "expires_at": b.ExpiresAt.UTC(), "seconds_left": int64(left.Seconds()), "hostnames": names,
	}
	s.event(ctx, b.Ref, "branch.expiring", payload)
	s.event(ctx, b.ParentRef, "branch.expiring", payload)
	s.log.Warn("a branch with a hostname expires soon", "ref", b.Ref, "name", b.Name, "expires_at", b.ExpiresAt.UTC().Format(time.RFC3339), "hostnames", strings.Join(names, ","))
	s.notifyExpiring(ctx, b, names)
	return true
}

// notifyExpiring posts the warning to the branch's notify_url in the envelope the run notifications
// use, signed the same way. A failure is logged and dropped.
func (s *Service) notifyExpiring(ctx context.Context, b *Branch, names []string) {
	if b.NotifyURL == "" {
		return
	}
	var e notifyEnvelope
	e.Type, e.Timestamp = "branch.expiring", rfc3339(s.now())
	e.Data.ProjectRef = b.Ref
	e.Data.DetailsURL = s.cfg.DashboardURL() + "/project/" + b.ParentRef + "/branches"
	e.Data.ActionRun = notifyActionRun{Steps: []notifyStep{}}
	e.Supavise = map[string]any{
		"branch_id": b.ID, "branch_name": b.Name, "project_ref": b.Ref, "parent_project_ref": b.ParentRef,
		"expires_at": rfc3339(*b.ExpiresAt), "hostnames": names,
		"message": "this branch is deleted at expires_at, and its custom or vanity hostname and its certificate with it; make it persistent to keep it",
	}
	body, _ := json.Marshal(e)
	nctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(nctx, http.MethodPost, b.NotifyURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "supavise-branching")
	id, ts := "msg_"+strings.ReplaceAll(secrets.NewUUID(), "-", ""), s.now().Unix()
	if secret, err := s.NotifySecret(nctx); err != nil {
		s.log.Warn("branch expiry notification is not signed", "ref", b.Ref, "err", err)
	} else if sig, err := signWebhook(secret, id, ts, body); err != nil {
		s.log.Warn("branch expiry notification is not signed", "ref", b.Ref, "err", err)
	} else {
		req.Header.Set("webhook-id", id)
		req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("webhook-signature", sig)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		s.log.Warn("branch expiry notification failed", "ref", b.Ref, "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		s.log.Warn("branch expiry notification refused", "ref", b.Ref, "status", resp.StatusCode)
	}
}
