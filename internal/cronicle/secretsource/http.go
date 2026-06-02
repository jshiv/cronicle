package secretsource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// HTTPSource pulls the name → value map from an HTTP(S) GET endpoint.
//
// Wire contract:
//
//	GET <url>
//	  Authorization: Bearer <token>  (optional)
//	  If-None-Match:  <prev-etag>    (optional)
//	→ 200 OK  with ETag: <opaque>  and body {"values":{NAME:"value", ...}}
//	→ 304 Not Modified  (etag matched, no body)
//	→ 401/403           (auth rejected; ErrUnauthorized)
//	→ 404               (source absent; ErrNotFound)
//	→ anything else     (transient error; refresh loop keeps prior cache)
//
// The `{values: {...}}` envelope (rather than a flat map) leaves room
// for future fields — next_refresh hints, advisories — without
// breaking parsers.
//
// Auth: bearer token via WithBearer or constructed from the
// CRONICLE_SECRET_SOURCE_TOKEN env at the caller. Tokens never appear
// in String() output.
//
// Transport: cronicle ships with the default Go http.Client (system
// TLS roots). For local-dev plaintext (in-cluster service DNS), an
// http:// URL works; for anything internet-facing, use https://.
type HTTPSource struct {
	URL     string
	Client  *http.Client
	Headers http.Header
}

// NewHTTPSource constructs an HTTPSource for the given URL. The
// default client has a 30s timeout — refresh ticks are seconds-scale
// so long-running fetches indicate trouble worth surfacing as a warn.
func NewHTTPSource(rawurl string) (*HTTPSource, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("invalid http url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("not an http(s) url: %s", rawurl)
	}
	return &HTTPSource{
		URL:     rawurl,
		Client:  &http.Client{Timeout: 30 * time.Second},
		Headers: http.Header{},
	}, nil
}

// WithBearer sets an Authorization header for every fetch. Returns
// the source for chaining at construction sites.
func (s *HTTPSource) WithBearer(token string) *HTTPSource {
	s.Headers.Set("Authorization", "Bearer "+token)
	return s
}

func (s *HTTPSource) Fetch(ctx context.Context, prevEtag string) (map[string]string, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, prevEtag, false, err
	}
	for k, vs := range s.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if prevEtag != "" {
		req.Header.Set("If-None-Match", prevEtag)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, prevEtag, false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, prevEtag, false, nil
	case http.StatusNotFound:
		return nil, prevEtag, false, ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, prevEtag, false, ErrUnauthorized
	case http.StatusOK:
		// fall through
	default:
		return nil, prevEtag, false,
			fmt.Errorf("secret source: %s: %s", redactURL(s.URL), resp.Status)
	}

	etag := resp.Header.Get("ETag")
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB ceiling — secret bundles aren't large
	if err != nil {
		return nil, prevEtag, false, err
	}
	var env struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, prevEtag, false, fmt.Errorf("secret source: decode body: %w", err)
	}
	if env.Values == nil {
		// Treat as empty rather than error — an authorized request that
		// returns no secrets is a legitimate empty state, not a fault.
		env.Values = map[string]string{}
	}
	if etag != "" && etag == prevEtag {
		// Server didn't honor If-None-Match but the etag is unchanged.
		// Treat as no-op so the refresh loop doesn't re-audit.
		return nil, etag, false, nil
	}
	return env.Values, etag, true, nil
}

func (s *HTTPSource) String() string {
	return redactURL(s.URL)
}

// redactURL strips userinfo and query string before any log emission.
// Query params on a secret-source URL are unlikely but if they exist
// they may contain identifying tokens — drop them defensively.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	return u.String()
}
