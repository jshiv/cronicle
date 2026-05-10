package state

import (
	"encoding/json"
	"os"
)

// jsonline marshals a map into a single JSONL line. Used by tests to
// construct events without long format-string tails.
func jsonline(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// mkdirAll wraps os.MkdirAll so the consumer test doesn't need an os
// import (kept tidy by intent).
func mkdirAll(p string, m uint32) error {
	return os.MkdirAll(p, os.FileMode(m))
}
