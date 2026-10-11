package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/domains"
	"github.com/supavise/supavise/internal/registry"
)

// TestPebbleCustomHostnameOnABranch takes a custom hostname of a branch from initialize to a certificate
// from Pebble, then follows the branch through a reset and a delete: during the reset window (the cluster
// removed and built again over the same row) the hostname still completes the TLS handshake with the same
// certificate and every request answers 503 with Retry-After from the proxy; when the branch is back the
// same hostname serves again, and when the branch is deleted its route and its certificate go. It is CI
// only, with the environment of TestPebbleCustomHostname (internal/proxy/pebble-test.sh).
func TestPebbleCustomHostnameOnABranch(t *testing.T) {
	dir := os.Getenv("SUPAVISE_TEST_PEBBLE_URL")
	mgmt := os.Getenv("SUPAVISE_TEST_CHALLTESTSRV_URL")
	dnsAddr := os.Getenv("SUPAVISE_TEST_CHALLTESTSRV_DNS")
	if dir == "" || mgmt == "" || dnsAddr == "" {
		t.Skip("SUPAVISE_TEST_PEBBLE_URL, SUPAVISE_TEST_CHALLTESTSRV_URL or _DNS not set (CI-only test, see internal/proxy/pebble-test.sh)")
	}
	httpPort := envPort(t, "SUPAVISE_TEST_PEBBLE_HTTP_PORT", 5002)
	tlsPort := envPort(t, "SUPAVISE_TEST_PEBBLE_TLS_PORT", 5001)
	const hostname = "preview.example.test"
	ctx := context.Background()

	var restHits atomic.Int64
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		restHits.Add(1)
		_, _ = io.WriteString(w, `{"service":"rest"}`)
	}))
	t.Cleanup(rest.Close)
	restAddr := strings.TrimPrefix(rest.URL, "http://")

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Domain, cfg.TLS.Mode = "example.test", "http01"
	cfg.TLS.CA, cfg.TLS.CACert, cfg.TLS.Email = dir, os.Getenv("SUPAVISE_TEST_PEBBLE_CA"), "ci@example.test"
	reg := registry.NewMemory()
	const branch = "bbbbbbbbbbbbbbbbbbbb"
	if err := reg.CreateProject(ctx, &registry.Project{Ref: testRef, Name: "p", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	if err := reg.CreateProject(ctx, &registry.Project{Ref: branch, Name: "feature", Status: registry.StatusActiveHealthy,
		Branch: &registry.BranchInfo{ID: "7c0c9c4e-0000-4000-8000-000000000001", ParentRef: testRef, Name: "feature", State: registry.BranchFunctionsDeployed}}); err != nil {
		t.Fatal(err)
	}
	keys := newFakeKeys()
	k := testKeys(t, branch)
	keys.set(branch, k)
	keys.set(testRef, testKeys(t, testRef))
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", dnsAddr)
	}}
	s, err := New(Options{Config: cfg, Registry: reg, Keys: keys, Logger: quietLog(), Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	s.upstreamFn = func(service, project) string { return restAddr }
	httpLn, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(httpPort))
	if err != nil {
		t.Fatal(err)
	}
	httpsLn, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(tlsPort))
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Serve(sctx, httpLn, httpsLn) }()
	defer func() { cancel(); <-done }()

	svc := domains.New(domains.Options{Reg: reg, Config: cfg, Resolver: resolver})
	handshake := func() (*tls.ConnectionState, error) {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", httpsLn.Addr().String(),
			&tls.Config{ServerName: hostname, InsecureSkipVerify: true})
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		st := conn.ConnectionState()
		return &st, nil
	}

	// Verified and activated on the branch.
	st, err := svc.Initialize(ctx, branch, hostname)
	if err != nil {
		t.Fatal(err)
	}
	if st.Target != branch+".api.example.test" {
		t.Fatalf("the CNAME target of the branch's hostname is %q", st.Target)
	}
	setTXT(t, mgmt, domains.ChallengeName(hostname)+".", st.TXTValue)
	if st, err = svc.Reverify(ctx, branch); err != nil || st.Status != registry.HostnameOriginReady {
		t.Fatalf("reverify: %+v %v", st, err)
	}
	if _, _, err := svc.Activate(ctx, branch); err != nil {
		t.Fatal(err)
	}
	var cs *tls.ConnectionState
	for end := time.Now().Add(90 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		if c, err := handshake(); err == nil && len(c.PeerCertificates) > 0 && len(c.PeerCertificates[0].DNSNames) > 0 && c.PeerCertificates[0].DNSNames[0] == hostname {
			cs = c
			break
		}
	}
	if cs == nil {
		t.Fatalf("no certificate for %s within 90 s of activating it on the branch", hostname)
	}
	serial := cs.PeerCertificates[0].SerialNumber

	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", httpsLn.Addr().String())
		},
		TLSClientConfig: &tls.Config{ServerName: hostname, InsecureSkipVerify: true},
	}}
	get := func() (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", "https://"+hostname+"/rest/v1/todos", nil)
		req.Header.Set("apikey", k.PublishableKey)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET through %s: %v", hostname, err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, resp.Header.Get("Retry-After")
	}
	if code, _ := get(); code != 200 {
		t.Fatalf("REST through the branch's hostname: %d", code)
	}

	// The reset window: the hostname keeps its certificate and answers 503 with Retry-After, and nothing
	// is dialed upstream, until the branch is up again; then it serves with the same certificate.
	for _, status := range []registry.Status{registry.StatusGoingDown, registry.StatusInitFailed, registry.StatusComingUp} {
		if err := reg.SetProjectStatus(ctx, branch, status); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the proxy to see "+string(status), func() bool {
			p, _ := s.table.hostProject(hostname)
			return p.status == status
		})
		hits := restHits.Load()
		code, retry := get()
		if code != 503 || retry == "" {
			t.Fatalf("during the reset window (%s): %d, Retry-After %q", status, code, retry)
		}
		if restHits.Load() != hits {
			t.Fatalf("a request reached the upstream while the branch was %s", status)
		}
		// The certificate is in the node's cache, so the handshake does not depend on the project's status.
		if c, err := handshake(); err != nil || c.PeerCertificates[0].SerialNumber.Cmp(serial) != 0 {
			t.Fatalf("the handshake during %s: %v", status, err)
		}
	}
	if err := reg.SetProjectStatus(ctx, branch, registry.StatusActiveHealthy); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the branch to serve again", func() bool { code, _ := get(); return code == 200 })
	if c, err := handshake(); err != nil || c.PeerCertificates[0].SerialNumber.Cmp(serial) != 0 {
		t.Fatalf("the certificate changed across the reset: %v", err)
	}

	// Deleting the branch frees the route and, with it, the certificate.
	if err := reg.DeleteProject(ctx, branch); err != nil {
		t.Fatal(err)
	}
	gone := false
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		if _, err := handshake(); err != nil {
			gone = true
			break
		}
	}
	if !gone {
		t.Fatal("the node still serves a certificate for the hostname of a deleted branch")
	}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}
