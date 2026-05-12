package configsource

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Open parses a config-source spec and returns the matching Source.
//
// Accepted forms:
//   - file:///abs/path/cronicle.hcl
//   - /abs/path/cronicle.hcl     (no scheme → treated as file)
//   - relative/path.hcl          (no scheme → treated as file)
//   - http://host/path
//   - https://host/path
//   - s3://bucket/key/path
//   - postgres://user@host/db?table=…&key=…
//   - postgresql://…             (alias of postgres)
//
// The no-scheme fallback preserves cronicle's existing
// `--path cronicle.hcl` ergonomics: anything without `://` is treated
// as a filesystem path. Anything with a scheme is dispatched by URL.
//
// ctx is only used by sources that need a network round-trip at
// construction time (currently just S3 and Postgres). File and HTTP
// sources ignore it.
func Open(ctx context.Context, spec string) (Source, error) {
	if spec == "" {
		return nil, fmt.Errorf("config source spec is empty")
	}
	if !strings.Contains(spec, "://") {
		// No scheme — treat as a local filesystem path.
		return NewFileSource(spec)
	}
	u, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid config source url: %w", err)
	}
	switch u.Scheme {
	case "file":
		// file:///abs/path → host="" path="/abs/path"
		// file://relative/path → host="relative" path="/path" — operator confusion;
		// reject and tell them to use a leading slash or no scheme at all.
		if u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf(
				"file:// requires an absolute path (use file:///path or just /path), got host=%q in %q",
				u.Host, spec)
		}
		return NewFileSource(u.Path)
	case "http", "https":
		return NewHTTPSource(spec)
	case "s3":
		return NewS3Source(ctx, spec)
	case "postgres", "postgresql":
		return NewPostgresSource(ctx, spec)
	default:
		return nil, fmt.Errorf("unsupported config source scheme %q in %q (want file/http/https/s3/postgres)",
			u.Scheme, spec)
	}
}
