package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client speaks GitHub's REST API. It holds no credentials: each call names the token it uses (the
// App's JWT, an installation token or a user's token), and a token is sent only to the API origin
// the client was built for.
type Client struct {
	HTTP *http.Client
	// API and Web are the API and web origins, no trailing slash.
	API, Web string
	// Version is the X-GitHub-Api-Version header.
	Version   string
	UserAgent string
}

// maxJSON bounds a JSON answer: a recursive tree of a large repository is the biggest.
const maxJSON = 64 << 20

// APIError is an answer of GitHub that is not a success.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
	// SSO is set when the organization requires SAML SSO authorization of the token.
	SSO bool
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsStatus reports whether err is a GitHub answer with this status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// bearer is the Authorization header of a token; "" sends none.
func bearer(token string) string {
	if token == "" {
		return ""
	}
	return "Bearer " + token
}

func (c *Client) newRequest(ctx context.Context, auth, method, path string, body io.Reader) (*http.Request, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("github: path %q is not absolute", path)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.API+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	v := c.Version
	if v == "" {
		v = "2022-11-28"
	}
	req.Header.Set("X-GitHub-Api-Version", v)
	ua := c.UserAgent
	if ua == "" {
		ua = "supavise"
	}
	req.Header.Set("User-Agent", ua)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// retryable is whether a GET that failed this way is worth asking again.
func retryable(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// send runs a request, retrying a GET twice on a gateway error, and returns the response with its
// body open. A status of 300 or more comes back as an *APIError and the body is closed.
func (c *Client) send(ctx context.Context, auth, method, path string, payload []byte, accept string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * 300 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := c.newRequest(ctx, auth, method, path, body)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := c.client().Do(req)
		if err != nil {
			lastErr = err
			if method == http.MethodGet && ctx.Err() == nil {
				continue
			}
			return nil, err
		}
		// The HTTP client does not follow redirects (a token must not leave the API's origin), so a
		// 301 or 307, which GitHub answers for a repository that was renamed or transferred, is an
		// error here and not a body to decode.
		if resp.StatusCode < 300 {
			return resp, nil
		}
		ae := readAPIError(resp, method, path)
		if method == http.MethodGet && retryable(resp.StatusCode) {
			lastErr = ae
			continue
		}
		return nil, ae
	}
	return nil, lastErr
}

func readAPIError(resp *http.Response, method, path string) *APIError {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(b, &m)
	return &APIError{Status: resp.StatusCode, Method: method, Path: pathOnly(path), Message: m.Message,
		SSO: resp.Header.Get("X-GitHub-SSO") != ""}
}

// pathOnly drops the query: an error message must not repeat a code or a token that rode in it.
func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// do sends in as JSON (nil for none) and decodes the JSON answer into out (nil to ignore it).
func (c *Client) do(ctx context.Context, token, method, path string, in, out any) error {
	return c.doAuth(ctx, bearer(token), method, path, in, out)
}

// doAuth is do with the whole Authorization header.
func (c *Client) doAuth(ctx context.Context, auth, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	resp, err := c.send(ctx, auth, method, path, payload, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSON)).Decode(out); err != nil {
		return fmt.Errorf("github: %s %s: decode the answer: %w", method, pathOnly(path), err)
	}
	return nil
}

// raw returns a body as GitHub's raw media type gives it, at most limit bytes; a longer one is an error.
func (c *Client) raw(ctx context.Context, token, path string, limit int64) ([]byte, error) {
	resp, err := c.send(ctx, bearer(token), http.MethodGet, path, nil, "application/vnd.github.raw+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("github: %s is larger than %d bytes", pathOnly(path), limit)
	}
	return b, nil
}

// getAll reads every page of a list. each gets the decoded page; key names the array inside an
// object answer (GitHub wraps some lists, such as installations, in one) and is empty for an
// array. It follows the next link only when it stays on the API origin, so a token never leaves it.
func (c *Client) getAll(ctx context.Context, token, path, key string, maxPages int, each func(raw json.RawMessage) error) error {
	next := path
	for page := 0; next != "" && page < maxPages; page++ {
		resp, err := c.send(ctx, bearer(token), http.MethodGet, next, nil, "")
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSON))
		resp.Body.Close()
		if err != nil {
			return err
		}
		list := json.RawMessage(body)
		if key != "" {
			var wrap map[string]json.RawMessage
			if err := json.Unmarshal(body, &wrap); err != nil {
				return fmt.Errorf("github: GET %s: decode the answer: %w", pathOnly(next), err)
			}
			list = wrap[key]
		}
		if err := each(list); err != nil {
			return err
		}
		next = c.nextLink(resp.Header.Get("Link"))
	}
	return nil
}

// nextLink returns the path and query of the rel="next" link, "" when there is none or it points
// away from the API origin.
func (c *Client) nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		seg := strings.Split(strings.TrimSpace(part), ";")
		if len(seg) < 2 {
			continue
		}
		isNext := false
		for _, p := range seg[1:] {
			if strings.TrimSpace(p) == `rel="next"` {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		raw := strings.Trim(strings.TrimSpace(seg[0]), "<>")
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		base, err := url.Parse(c.API)
		if err != nil {
			return ""
		}
		if u.IsAbs() && (u.Scheme != base.Scheme || u.Host != base.Host) {
			return ""
		}
		p := u.EscapedPath()
		if bp := strings.TrimRight(base.EscapedPath(), "/"); bp != "" {
			p = strings.TrimPrefix(p, bp)
		}
		if u.RawQuery != "" {
			p += "?" + u.RawQuery
		}
		return p
	}
	return ""
}

// escapeSegments escapes a repository path or a branch name for a URL path, keeping its slashes.
func escapeSegments(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
