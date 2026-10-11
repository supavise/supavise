package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The static half of the Studio gate. studio/calls.json lists the Management API calls the pinned Studio source
// makes (studio/scan-calls.mjs; the method is in docs/reference/studio-platform-calls.md). This test joins it
// with the routes of the server: a call that no hand-written handler answers is answered by a spec-derived stub
// (an empty list, an empty object) or by the unknown-route fallback, and Studio takes either for "it worked" or
// "there is nothing". Such a call must be in tests/linux/crawl/allowlist.json with a reason, and an entry that
// is not a stub-answered call any more must be taken out, so the list only shrinks. The runtime crawl
// (tests/linux/studio-crawl.sh) reads the same file and fails on a stub it has not been told about.

const (
	studioCallsFile = "../../studio/calls.json"
	crawlAllowFile  = "../../tests/linux/crawl/allowlist.json"
)

type studioCalls struct {
	Calls []struct {
		Method string   `json:"method"`
		Path   string   `json:"path"`
		Files  []string `json:"files"`
	} `json:"calls"`
	Mentions []struct {
		Path  string   `json:"path"`
		Files []string `json:"files"`
	} `json:"mentions"`
}

// crawlAllow is tests/linux/crawl/allowlist.json. Reach says how an entry is verified: "crawl" entries must be
// seen by the runtime crawl (a stub nobody hits is a stale entry), "static" entries are calls the crawl cannot
// reach (a page that needs a feature this node does not have) and are checked here only.
type crawlAllow struct {
	Stubs []struct {
		Op     string `json:"op"`
		Reach  string `json:"reach"`
		Reason string `json:"reason"`
	} `json:"stubs"`
	Statuses []struct {
		Op     string `json:"op"`
		Status int    `json:"status"`
		Reach  string `json:"reach"`
		Reason string `json:"reason"`
	} `json:"statuses"`
	DOM []struct {
		Route  string `json:"route"`
		Text   string `json:"text"`
		Reach  string `json:"reach"`
		Reason string `json:"reason"`
	} `json:"dom"`
}

func readJSON(t *testing.T, file string, into any) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
}

var paramRe = regexp.MustCompile(`\{[^}]*\}`)

// normOp is "METHOD /path" with every path parameter written {}, so that {ref} and {projectRef} compare equal.
func normOp(method, path string) string { return method + " " + paramRe.ReplaceAllString(path, "{}") }

// routeKind is how the server answers a request: "impl" (a hand-written handler), "stub" (the stub of a spec
// operation), "fallback" (the unknown-route handler: a stub with the X-Supavise-Stub header under /platform,
// a 404 elsewhere).
type routeKind struct {
	Kind    string
	Pattern string
}

// routeTable rebuilds the mux of Server.build with a marker for a handler, in the same order, so that the
// match for a request is the one the server makes.
func routeTable(t *testing.T, s *Server) func(method, path string) routeKind {
	t.Helper()
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	impl := s.implemented()
	mark := func(kind, pattern string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Route-Kind", kind)
			w.Header().Set("X-Route-Pattern", pattern)
		})
	}
	mux := &muxSet{}
	inSpec := map[string]bool{}
	for _, op := range ops {
		key := op.Key()
		inSpec[key] = true
		if _, ok := impl[key]; ok {
			mux.handle(key, mark("impl", key))
			continue
		}
		mux.handle(key, mark("stub", key))
	}
	extra := make([]string, 0, len(impl))
	for key := range impl {
		if !inSpec[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	for _, key := range extra {
		mux.handle(key, mark("impl", key))
	}
	mux.fallback = mark("fallback", "")
	return func(method, path string) routeKind {
		concrete := paramRe.ReplaceAllString(path, "p0x1")
		r := httptest.NewRequest(method, concrete, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return routeKind{Kind: w.Header().Get("X-Route-Kind"), Pattern: w.Header().Get("X-Route-Pattern")}
	}
}

// stubAnswered returns the "METHOD /path" of every call of calls.json that no handler answers, with the key
// the allow-list names it by. A mention (a path in the source that is not the argument of a call the scan
// understands, with no method) counts when no method of that path has a handler.
func stubAnswered(t *testing.T) map[string]string {
	t.Helper()
	f := newFixture(t)
	route := routeTable(t, f.srv)
	var calls studioCalls
	readJSON(t, studioCallsFile, &calls)
	if len(calls.Calls) < 100 {
		t.Fatalf("%s lists %d calls; regenerate it with `node studio/scan-calls.mjs --fetch`", studioCallsFile, len(calls.Calls))
	}
	out := map[string]string{}
	for _, c := range calls.Calls {
		k := route(c.Method, c.Path)
		if k.Kind == "impl" {
			continue
		}
		op := c.Method + " " + c.Path
		if k.Pattern != "" {
			op = k.Pattern // the spec's spelling, and its method: a HEAD is answered by the route of a GET
		}
		method, path, _ := strings.Cut(op, " ")
		out[normOp(method, path)] = op
	}
	for _, m := range calls.Mentions {
		handled := false
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"} {
			if route(method, m.Path).Kind == "impl" {
				handled = true
			}
		}
		if !handled {
			out["* "+paramRe.ReplaceAllString(m.Path, "{}")] = "* " + m.Path
		}
	}
	return out
}

func TestStudioCallsAreAnsweredByHandlers(t *testing.T) {
	var allow crawlAllow
	readJSON(t, crawlAllowFile, &allow)
	listed := map[string]bool{}
	for _, e := range allow.Stubs {
		method, path, _ := strings.Cut(e.Op, " ")
		listed[normOp(method, path)] = true
	}
	stubs := stubAnswered(t)

	var missing, stale []string
	for norm, op := range stubs {
		if !listed[norm] {
			missing = append(missing, op)
		}
	}
	for _, e := range allow.Stubs {
		method, path, _ := strings.Cut(e.Op, " ")
		if _, still := stubs[normOp(method, path)]; !still {
			stale = append(stale, e.Op)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("Studio calls that only a stub answers (a handler regressed to a stub, or Studio gained a call). "+
			"Write a handler, or list the call with a reason under \"stubs\" in %s:\n  %s", crawlAllowFile, strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("allow-list entries for calls a handler answers now (or that Studio does not make): remove them from %s:\n  %s",
			crawlAllowFile, strings.Join(stale, "\n  "))
	}
}

// The file is read by the crawl too: a typo in an entry would make the crawl fail for a reason that is not the node's.
func TestCrawlAllowlistIsWellFormed(t *testing.T) {
	var allow crawlAllow
	readJSON(t, crawlAllowFile, &allow)
	op := regexp.MustCompile(`^(?:\*|GET|POST|PUT|PATCH|DELETE|HEAD) /\S+$`)
	seen := map[string]bool{}
	check := func(kind, key, reach, reason string) {
		if reason == "" || len(strings.Fields(reason)) < 3 {
			t.Errorf("%s %q: the reason says nothing", kind, key)
		}
		if reach != "crawl" && reach != "static" {
			t.Errorf("%s %q: reach is %q, want \"crawl\" or \"static\"", kind, key, reach)
		}
		if seen[kind+key] {
			t.Errorf("%s %q is listed twice", kind, key)
		}
		seen[kind+key] = true
	}
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	inSpec := map[string]bool{}
	for _, o := range ops {
		inSpec[normOp(o.Method, o.Path)] = true
	}
	f := newFixture(t)
	for key := range f.srv.implemented() {
		method, path, _ := strings.Cut(key, " ")
		inSpec[normOp(method, path)] = true
	}
	for _, e := range allow.Stubs {
		if !op.MatchString(e.Op) {
			t.Errorf("stubs: %q is not \"METHOD /path\"", e.Op)
		}
		check("stubs", e.Op, e.Reach, e.Reason)
	}
	for _, e := range allow.Statuses {
		if !op.MatchString(e.Op) {
			t.Errorf("statuses: %q is not \"METHOD /path\"", e.Op)
		}
		if e.Status < 400 || e.Status > 599 {
			t.Errorf("statuses %q: %d is not an error status", e.Op, e.Status)
		}
		check("statuses", e.Op+" "+http.StatusText(e.Status), e.Reach, e.Reason)
		// A status entry for the Management API names a route that exists; the others (Studio's own /api
		// routes, the project hosts) are outside this server's route table.
		method, path, _ := strings.Cut(e.Op, " ")
		managed := strings.HasPrefix(path, "/platform/") || strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v2/")
		if managed && method != "*" && !inSpec[normOp(method, path)] {
			t.Errorf("statuses %q: no route of the server has this method and path", e.Op)
		}
	}
	for _, e := range allow.DOM {
		if e.Route == "" || e.Text == "" {
			t.Errorf("dom: an entry needs a route and a text: %+v", e)
		}
		check("dom", e.Route+" "+e.Text, e.Reach, e.Reason)
	}
}

// Every honest refusal of unsupported.go is a 4xx or 5xx that the crawl sees when Studio asks, so it must be
// allow-listed with its status. A refusal added there without an entry would fail the crawl; this fails first.
func TestUnsupportedRefusalsAreAllowListed(t *testing.T) {
	src, err := os.ReadFile("unsupported.go")
	if err != nil {
		t.Fatal(err)
	}
	var allow crawlAllow
	readJSON(t, crawlAllowFile, &allow)
	listed := map[string]bool{}
	for _, e := range allow.Statuses {
		method, path, _ := strings.Cut(e.Op, " ")
		listed[normOp(method, path)+" "+http.StatusText(e.Status)] = true
	}
	statusOf := map[string]int{"http.StatusBadRequest": 400, "http.StatusNotFound": 404, "http.StatusNotImplemented": 501, "http.StatusForbidden": 403, "http.StatusConflict": 409}
	re := regexp.MustCompile(`add\((?:"([A-Z]+) |m\+" )(/[^"]+)", s\.refuse\((http\.Status\w+)`)
	methodsRe := regexp.MustCompile(`range \[\]string\{([^}]*)\}`)
	lines := strings.Split(string(src), "\n")
	n := 0
	for i, line := range lines {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		methods := []string{m[1]}
		if m[1] == "" {
			for j := i - 1; j >= 0 && j > i-4; j-- {
				if mm := methodsRe.FindStringSubmatch(lines[j]); mm != nil {
					methods = nil
					for _, q := range strings.Split(mm[1], ",") {
						methods = append(methods, strings.Trim(strings.TrimSpace(q), `"`))
					}
					break
				}
			}
		}
		status, ok := statusOf[m[3]]
		if !ok {
			t.Fatalf("unsupported.go:%d: add the status %s to this test", i+1, m[3])
		}
		for _, method := range methods {
			n++
			if !listed[normOp(method, m[2])+" "+http.StatusText(status)] {
				t.Errorf("unsupported.go refuses %s %s with %d, and %s has no \"statuses\" entry for it", method, m[2], status, crawlAllowFile)
			}
		}
	}
	if n < 6 {
		t.Fatalf("found %d refusals in unsupported.go; the pattern in this test is out of date", n)
	}
}
