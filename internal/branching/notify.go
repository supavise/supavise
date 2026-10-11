package branching

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// validNotifyURL accepts an absolute http or https URL. Where it points is checked again
// when the call is made, at connect time, so that DNS cannot change the answer in between.
func validNotifyURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return invalid("notify_url %q must be an absolute http or https URL", raw)
	}
	return nil
}

// reservedPrefixes are ranges netip's predicates do not cover that still reach internal
// services: shared address space (RFC 6598, where some clouds put their metadata service,
// for example 100.100.100.200), the benchmarking range, IETF protocol assignments, the
// reserved class E block, and the NAT64 prefix (which translates to IPv4, private addresses
// included).
var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
}

// internalAddr reports whether ip (already unmapped from IPv4-in-IPv6) is an address a
// notify_url must not reach.
func internalAddr(ip netip.Addr) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	for _, p := range reservedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// newNotifyClient returns the client for notify_url calls. Unless allowPrivate is set it
// refuses to connect to loopback, private, link-local and unspecified addresses, which would
// make the daemon a way into the instance metadata service and the node's own admin ports.
func newNotifyClient(allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			if ip = ip.Unmap(); internalAddr(ip) {
				return fmt.Errorf("branching: notify_url resolves to %s, a private or reserved address ([branching] allow_private_notify_urls)", ip)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{DialContext: d.DialContext, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// notifySecretName is the sealed project secret, held by the system project, that signs the
// notify_url calls of the node.
const notifySecretName = "branch_notify_signing_key"

// NotifySecret returns the secret that signs the calls to every branch's notify_url (a Standard
// Webhooks secret: "whsec_" and the base64 of 32 random bytes). The node creates it when first
// asked. A receiver verifies the webhook-signature header with it
// (https://www.standardwebhooks.com).
func (s *Service) NotifySecret(ctx context.Context) (string, error) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	open := func() (string, error) {
		sealed, err := s.reg.GetSecret(ctx, config.SystemRef, notifySecretName)
		if err != nil {
			return "", err
		}
		pt, err := s.sec.Open(sealed)
		return string(pt), err
	}
	key, err := open()
	if !errors.Is(err, registry.ErrNotFound) {
		return key, err
	}
	if key, err = newNotifySecret(); err != nil {
		return "", err
	}
	sealed, err := s.sec.Seal([]byte(key))
	if err != nil {
		return "", err
	}
	if _, err := registry.PutSecretIfAbsent(ctx, s.reg, config.SystemRef, notifySecretName, sealed); err != nil {
		return "", fmt.Errorf("store the notify signing key: %w", err)
	}
	return open() // the first writer wins, whoever that was
}

// RotateNotifySecret replaces the notify signing key and returns the new one. Receivers must
// take the new key before the next notification.
func (s *Service) RotateNotifySecret(ctx context.Context) (string, error) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	key, err := newNotifySecret()
	if err != nil {
		return "", err
	}
	sealed, err := s.sec.Seal([]byte(key))
	if err != nil {
		return "", err
	}
	return key, s.reg.PutSecret(ctx, config.SystemRef, notifySecretName, sealed)
}

func newNotifySecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(b), nil
}

// signWebhook returns the Standard Webhooks signature header value for a message:
// "v1," and the base64 HMAC-SHA256, keyed by the secret's decoded bytes, of "id.timestamp.body".
func signWebhook(secret, id string, ts int64, body []byte) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return "", fmt.Errorf("the notify signing key is not a Standard Webhooks secret: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%d.", id, ts)
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// notifyEnvelope is what a notify_url receives when a run ends: the envelope hosted Supabase
// sends (https://supabase.com/docs/guides/deployment/branching/working-with-branches), with what
// this node used to send before the envelope existed under "supavise".
type notifyEnvelope struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Data      struct {
		ProjectRef string          `json:"project_ref"`
		DetailsURL string          `json:"details_url"`
		ActionRun  notifyActionRun `json:"action_run"`
	} `json:"data"`
	Supavise map[string]any `json:"supavise"`
}

type notifyActionRun struct {
	ID        string       `json:"id"`
	CreatedAt string       `json:"created_at"`
	Steps     []notifyStep `json:"steps"`
	// Failure is why the run failed; absent after a success.
	Failure string `json:"failure,omitempty"`
}

type notifyStep struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // lowercase: exited, paused, dead
	UpdatedAt string `json:"updated_at"`
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// notifyBody builds the envelope for a run that ended.
func (s *Service) notifyBody(p *registry.Project, rec *registry.BranchRun, op, state, detail string) notifyEnvelope {
	var e notifyEnvelope
	e.Type, e.Timestamp = "run.completed", rfc3339(s.now())
	e.Data.ProjectRef = p.Ref
	e.Data.DetailsURL = s.cfg.DashboardURL() + "/project/" + p.Branch.ParentRef + "/branches"
	e.Data.ActionRun = notifyActionRun{ID: rec.ID, CreatedAt: rfc3339(rec.CreatedAt), Steps: []notifyStep{}, Failure: rec.Failure}
	for _, st := range rec.Steps {
		e.Data.ActionRun.Steps = append(e.Data.ActionRun.Steps, notifyStep{Name: st.Name, Status: strings.ToLower(st.Status), UpdatedAt: rfc3339(st.UpdatedAt)})
	}
	e.Supavise = map[string]any{
		"branch_id": p.Branch.ID, "branch_name": p.Branch.Name, "project_ref": p.Ref, "parent_project_ref": p.Branch.ParentRef,
		"operation": op, "status": state, "message": detail, "project_status": p.Status,
	}
	return e
}

// notify tells the branch's notify_url how an operation ended. Failures are logged and
// dropped: a webhook must not fail a merge.
func (s *Service) notify(ctx context.Context, ref string, r *run, state registry.BranchState, detail string) {
	p, err := s.reg.GetProject(ctx, ref)
	if err != nil || p.Branch == nil || p.Branch.NotifyURL == "" {
		return
	}
	rec := &registry.BranchRun{ID: r.id, Status: registry.RunSuccess, CreatedAt: s.now()}
	if st := s.runStore(); st != nil {
		if got, err := st.GetBranchRun(ctx, r.id); err == nil {
			rec = got
		}
	}
	body, _ := json.Marshal(s.notifyBody(p, rec, r.op, string(state), detail))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Branch.NotifyURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "supavise-branching")
	// Standard Webhooks headers, signed with the node's secret. A node that cannot read its secret
	// still delivers: the receiver decides what an unsigned call is worth.
	id, ts := "msg_"+strings.ReplaceAll(r.id, "-", ""), s.now().Unix()
	if secret, err := s.NotifySecret(ctx); err != nil {
		s.log.Warn("branch notification is not signed", "ref", ref, "err", err)
	} else if sig, err := signWebhook(secret, id, ts, body); err != nil {
		s.log.Warn("branch notification is not signed", "ref", ref, "err", err)
	} else {
		req.Header.Set("webhook-id", id)
		req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("webhook-signature", sig)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		s.log.Warn("branch notification failed", "ref", ref, "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		s.log.Warn("branch notification refused", "ref", ref, "status", resp.StatusCode)
	}
}
