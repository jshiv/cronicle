package configsource

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// HTTPSource pulls cronicle.hcl from an HTTP(S) URL. The etag protocol
// is the standard one: server emits an `ETag` (or `X-Cronicle-Config-
// Version`) response header on the initial GET; subsequent fetches
// send `If-None-Match: <etag>` and the server can short-circuit with
// 304 Not Modified. Falls back gracefully if the server doesn't
// participate in conditional GETs — we'll just always-redownload.
//
// Auth: the caller can supply a bearer token via WithBearer; or set
// arbitrary headers via WithHeader. The token is held in process
// memory and is NOT included in String() output.
type HTTPSource struct {
	URL     string
	Client  *http.Client
	Headers http.Header
}

// NewHTTPSource constructs an HTTPSource for the given URL. The
// default client has a 30s timeout — refresh ticks are sub-second so
// long-running fetches indicate trouble worth surfacing as a warn.
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

// WithHeader sets an arbitrary header for every fetch. Replaces any
// previous value for the same key.
func (s *HTTPSource) WithHeader(key, value string) *HTTPSource {
	s.Headers.Set(key, value)
	return s
}

func (s *HTTPSource) Fetch(ctx context.Context, prevEtag string) ([]byte, string, bool, error) {
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
		// Server confirms our cached etag is still current.
		return nil, prevEtag, false, nil
	case http.StatusNotFound:
		return nil, prevEtag, false, ErrNotFound
	case http.StatusOK:
		// fall through
	default:
		return nil, prevEtag, false,
			fmt.Errorf("http source: %s: %s", s.URL, resp.Status)
	}

	// Etag header is canonical; fall back to cronicle-infra's version
	// header when present so we don't always-redownload against an
	// api that emits the version but not the etag.
	etag := resp.Header.Get("ETag")
	if etag == "" {
		etag = resp.Header.Get("X-Cronicle-Config-Version")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, prevEtag, false, err
	}
	if etag != "" && etag == prevEtag {
		// Server didn't honor If-None-Match but the etag is unchanged.
		// Skip the parse step.
		return nil, etag, false, nil
	}
	return body, etag, true, nil
}

func (s *HTTPSource) String() string {
	// Redact userinfo before logging.
	if u, err := url.Parse(s.URL); err == nil {
		u.User = nil
		return u.String()
	}
	return s.URL
}
