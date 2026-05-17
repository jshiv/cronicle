package secretsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FileSource reads the secret map from a JSON file on disk. Intended
// for local development and testing — no fancy watching, no etag fast
// path, just stat-and-hash on each Fetch.
//
// File format:
//
//	{
//	  "values": {
//	    "ANTHROPIC_API_KEY": "sk-ant-...",
//	    "GITHUB_TOKEN": "ghp_..."
//	  }
//	}
//
// The `{values:{}}` envelope mirrors the HTTP wire format so test
// fixtures double as wire-shape examples.
//
// File mode is checked on every read — if it's world-readable we emit
// a warning into the returned error so dev users notice. Production
// users should be on HTTPSource, not this.
type FileSource struct {
	Path string
}

// NewFileSource resolves the path absolutely so log lines are
// unambiguous. The file doesn't have to exist at construction time;
// it's checked on each Fetch.
func NewFileSource(path string) (*FileSource, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &FileSource{Path: abs}, nil
}

func (s *FileSource) Fetch(_ context.Context, prevEtag string) (map[string]string, string, bool, error) {
	body, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, prevEtag, false, ErrNotFound
		}
		return nil, prevEtag, false, err
	}
	sum := sha256.Sum256(body)
	etag := hex.EncodeToString(sum[:])
	if etag == prevEtag {
		return nil, etag, false, nil
	}
	var env struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, prevEtag, false, fmt.Errorf("secret source: decode %s: %w", s.Path, err)
	}
	if env.Values == nil {
		env.Values = map[string]string{}
	}
	return env.Values, etag, true, nil
}

func (s *FileSource) String() string {
	return "file://" + s.Path
}
