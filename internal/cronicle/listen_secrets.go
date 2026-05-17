package cronicle

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
)

// Listener endpoints for the in-tree secret store (state.Backend's
// SecretStore role).
//
//   GET    /v1/secrets               → {"values":{NAME:value, ...}, "etag":"..."}
//                                       (bearer + If-None-Match honored)
//   PUT    /v1/secrets/{name}        body: {"value":"..."}  → upsert
//   DELETE /v1/secrets/{name}        → remove
//
// All endpoints require the listener bearer token, same as the rest of
// the API. Plaintext flows over the wire on GET and PUT — operators
// should pair this with TLS at any non-trusted hop (the listener
// itself doesn't terminate TLS; that's the deployment's responsibility,
// same as for /v1/jobs).

// validSecretName mirrors the convention used by cronicle-web and the
// $secret.NAME regex in secretstore — UPPER_SNAKE_CASE, leading letter.
// Anything else gets rejected at the listener so we don't accidentally
// admit lowercase/punctuation names that won't resolve via
// $secret.X at dispatch time.
var validSecretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func (s *listenServer) handleSecretsRoot(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	be := s.stateSrc()
	if be == nil {
		http.Error(w, `{"error":"state backend not bound"}`, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		etag, err := be.SecretsEtag()
		if err != nil {
			http.Error(w, jsonError(err), http.StatusInternalServerError)
			return
		}
		if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		values, err := be.ListSecrets()
		if err != nil {
			http.Error(w, jsonError(err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Values map[string]string `json:"values"`
			ETag   string            `json:"etag"`
		}{values, etag})
	default:
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *listenServer) handleSecretByName(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	be := s.stateSrc()
	if be == nil {
		http.Error(w, `{"error":"state backend not bound"}`, http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/secrets/")
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, `{"error":"secret name required (path: /v1/secrets/{name})"}`, http.StatusBadRequest)
		return
	}
	if !validSecretName.MatchString(name) {
		http.Error(w, `{"error":"name must match UPPER_SNAKE_CASE"}`, http.StatusBadRequest)
		return
	}

	actor := r.Header.Get("X-Cronicle-Actor")
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Value string `json:"value"`
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, jsonError(err), http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, jsonError(err), http.StatusBadRequest)
			return
		}
		if body.Value == "" {
			http.Error(w, `{"error":"value required"}`, http.StatusBadRequest)
			return
		}
		if err := be.PutSecret(name, body.Value, actor); err != nil {
			http.Error(w, jsonError(err), http.StatusInternalServerError)
			return
		}
		slog.Info("listener: secret upserted", "name", name, "actor", actor)
		etag, _ := be.SecretsEtag()
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNoContent)

	case http.MethodDelete:
		ok, err := be.DeleteSecret(name, actor)
		if err != nil {
			http.Error(w, jsonError(err), http.StatusInternalServerError)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		slog.Info("listener: secret deleted", "name", name, "actor", actor)
		etag, _ := be.SecretsEtag()
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.Header().Set("Allow", "PUT, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func jsonError(err error) string {
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{err.Error()})
	return string(b)
}
