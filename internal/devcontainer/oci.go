// OCI Distribution v2 read path used by Phase 2b's Feature fetch. The
// scope is intentionally narrow: anonymous bearer-token handshake from
// WWW-Authenticate, single-layer manifest fetch, blob fetch, tar extract
// with path safety. We hand-roll this rather than vendoring
// `go-containerregistry` because the read path is small (~200 lines)
// while the library would pull ~40 transitive deps (OpenTelemetry,
// logrus, oauth2, docker/cli, moby/moby, image-spec) for the same two
// HTTP GETs.

package devcontainer

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// manifestAccept is the Accept header for a single-platform OCI image
	// manifest. Devcontainer Features publish exactly one of these per
	// reference, never an index — picking from a manifest list is out of
	// scope.
	manifestAccept = "application/vnd.oci.image.manifest.v1+json"

	// featureLayerMediaType is the mediaType the devcontainer Features
	// distribution spec assigns to the layer carrying the Feature
	// tarball. We sanity-check it so an unrelated artifact published
	// under the same ref doesn't get silently extracted.
	featureLayerMediaType = "application/vnd.devcontainers.layer.v1+tar"

	// fetchTimeout caps the whole pull. Bounds anonymous tries against
	// an unreachable registry without users having to ^C `ahjo repo add`.
	fetchTimeout = 90 * time.Second
)

// FeatureRef is a parsed `<registry>/<repo>:<tag>` (or `@<digest>`).
// Empty Reference defaults to "latest" at parse time.
type FeatureRef struct {
	Registry   string // host[:port]
	Repository string // slash-separated path, e.g. devcontainers/features/node
	Reference  string // tag or sha256:<hex>
}

// String renders ref back into the canonical addressing form.
func (r FeatureRef) String() string {
	if strings.HasPrefix(r.Reference, "sha256:") {
		return r.Registry + "/" + r.Repository + "@" + r.Reference
	}
	return r.Registry + "/" + r.Repository + ":" + r.Reference
}

// ParseFeatureRef splits a devcontainer feature ID like
// `ghcr.io/devcontainers/features/node:1` into its OCI distribution
// addressing parts.
//
// Tag defaults to "latest" when absent. Digest references
// (`@sha256:...`) are supported; tag and digest are mutually exclusive
// per OCI grammar. A `:` before the first `/` is treated as host:port,
// not a tag.
func ParseFeatureRef(s string) (FeatureRef, error) {
	if s == "" {
		return FeatureRef{}, errors.New("empty feature reference")
	}
	// Digest form: <host>/<path>@sha256:<hex>
	if i := strings.LastIndex(s, "@"); i > 0 {
		head, digest := s[:i], s[i+1:]
		host, path, err := splitHostAndPath(head)
		if err != nil {
			return FeatureRef{}, err
		}
		if !strings.HasPrefix(digest, "sha256:") {
			return FeatureRef{}, fmt.Errorf("digest must be sha256:<hex>: %q", digest)
		}
		return FeatureRef{Registry: host, Repository: path, Reference: digest}, nil
	}
	// Tag form: <host>/<path>:<tag>. The last `:` is a tag separator
	// only when no `/` follows it (otherwise it's part of host:port).
	head, tag := s, "latest"
	if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s[i:], "/") {
		head, tag = s[:i], s[i+1:]
		if tag == "" {
			return FeatureRef{}, fmt.Errorf("empty tag in %q", s)
		}
	}
	host, path, err := splitHostAndPath(head)
	if err != nil {
		return FeatureRef{}, err
	}
	return FeatureRef{Registry: host, Repository: path, Reference: tag}, nil
}

func splitHostAndPath(s string) (host, path string, err error) {
	i := strings.Index(s, "/")
	if i <= 0 {
		return "", "", fmt.Errorf("expected <host>/<path>: %q", s)
	}
	if !strings.Contains(s[:i], ".") && !strings.Contains(s[:i], ":") && s[:i] != "localhost" {
		return "", "", fmt.Errorf("first segment %q is not a registry host (need a `.`, `:` or `localhost`)", s[:i])
	}
	return s[:i], s[i+1:], nil
}

// Fetcher fetches Feature OCI artifacts. Production callers use the
// zero-value http.DefaultClient; tests inject a recording client.
type Fetcher struct {
	HTTP *http.Client
}

// httpClient is the *http.Client to use, defaulting to http.DefaultClient.
func (f *Fetcher) httpClient() *http.Client {
	if f != nil && f.HTTP != nil {
		return f.HTTP
	}
	return http.DefaultClient
}

// Fetch pulls the Feature artifact at ref into dst (created with 0o755).
// Anonymous read; ghcr.io public Features need no auth beyond the
// standard 401-then-token-exchange handshake the registry guides us
// through.
//
// dst must not pre-exist with conflicting contents — extraction will
// happily overwrite files at their tar-declared paths, but won't clean
// up unrelated entries. Production callers pass a fresh os.MkdirTemp
// dir so a partial extraction can't poison subsequent fetches.
func (f *Fetcher) Fetch(ctx context.Context, ref FeatureRef, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.Registry, ref.Repository, url.PathEscape(ref.Reference))
	manifest, err := f.getJSON(ctx, manifestURL, manifestAccept)
	if err != nil {
		return fmt.Errorf("fetch manifest %s: %w", ref, err)
	}
	var m struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return fmt.Errorf("parse manifest %s: %w", ref, err)
	}
	if len(m.Layers) != 1 {
		return fmt.Errorf("feature %s manifest has %d layers; expected 1", ref, len(m.Layers))
	}
	layer := m.Layers[0]
	if layer.MediaType != featureLayerMediaType {
		return fmt.Errorf("feature %s layer mediaType %q; expected %q", ref, layer.MediaType, featureLayerMediaType)
	}
	blobURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.Registry, ref.Repository, layer.Digest)
	rc, err := f.getStream(ctx, blobURL, "")
	if err != nil {
		return fmt.Errorf("fetch blob %s: %w", ref, err)
	}
	defer rc.Close()
	if err := extractTar(rc, dst); err != nil {
		return fmt.Errorf("extract %s: %w", ref, err)
	}
	return nil
}

// getJSON fetches url and returns the body, transparently completing a
// 401/WWW-Authenticate bearer-token handshake when the server challenges.
// Retries the original request once with the issued bearer token.
func (f *Fetcher) getJSON(ctx context.Context, url, accept string) ([]byte, error) {
	rc, err := f.getStream(ctx, url, accept)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// getStream is getJSON with a streaming body, used for blobs we'd
// rather not buffer wholesale.
func (f *Fetcher) getStream(ctx context.Context, url, accept string) (io.ReadCloser, error) {
	resp, err := f.do(ctx, url, accept, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp.Body, nil
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return nil, drainErr(resp, "GET "+url)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	realm, params, err := parseBearerChallenge(challenge)
	if err != nil {
		return nil, fmt.Errorf("auth challenge from %s: %w", url, err)
	}
	token, err := f.fetchToken(ctx, realm, params)
	if err != nil {
		return nil, err
	}
	resp, err = f.do(ctx, url, accept, token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp.Body, nil
	}
	return nil, drainErr(resp, "GET "+url+" (after token)")
}

func (f *Fetcher) do(ctx context.Context, url, accept, bearer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return f.httpClient().Do(req)
}

// drainErr reads up to 1 KiB of resp.Body for an error message and
// returns a wrapped error. resp.Body is closed.
func drainErr(resp *http.Response, prefix string) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("%s: HTTP %d: %s", prefix, resp.StatusCode, strings.TrimSpace(string(body)))
}

// parseBearerChallenge handles the Bearer challenge subset of RFC 7235:
//
//	Bearer realm="https://x", service="y", scope="repository:foo:pull"
//
// Token (Basic, Negotiate, etc.) and parameter forms outside this
// grammar are rejected.
func parseBearerChallenge(h string) (realm string, params map[string]string, err error) {
	rest := strings.TrimSpace(h)
	if !strings.HasPrefix(strings.ToLower(rest), "bearer ") {
		return "", nil, fmt.Errorf("not a Bearer challenge: %q", h)
	}
	rest = strings.TrimSpace(rest[len("bearer "):])
	params = map[string]string{}
	for _, kv := range splitChallengeParams(rest) {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(kv[:eq])
		v := strings.Trim(strings.TrimSpace(kv[eq+1:]), `"`)
		params[k] = v
	}
	realm = params["realm"]
	if realm == "" {
		return "", nil, fmt.Errorf("missing realm in challenge %q", h)
	}
	return realm, params, nil
}

// splitChallengeParams splits on commas that are not inside quotes.
func splitChallengeParams(s string) []string {
	var out []string
	inQuote := false
	last := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case ',':
			if !inQuote {
				out = append(out, strings.TrimSpace(s[last:i]))
				last = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(s[last:]))
	return out
}

// fetchToken does the bearer-token exchange against realm with the
// service+scope params from the challenge.
func (f *Fetcher) fetchToken(ctx context.Context, realm string, params map[string]string) (string, error) {
	q := url.Values{}
	if v := params["service"]; v != "" {
		q.Set("service", v)
	}
	if v := params["scope"]; v != "" {
		q.Set("scope", v)
	}
	target := realm
	if enc := q.Encode(); enc != "" {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target = target + sep + enc
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return "", err
	}
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", drainErr(resp, "token GET "+target)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if t.Token != "" {
		return t.Token, nil
	}
	if t.AccessToken != "" {
		return t.AccessToken, nil
	}
	return "", fmt.Errorf("token response had neither `token` nor `access_token`")
}

// extractTar reads a plain tar (no gzip — the devcontainers spec's
// `application/vnd.devcontainers.layer.v1+tar` is uncompressed despite
// the .tgz filename hint registries put in OCI artifact annotations)
// into dst. Rejects entries that escape via:
//
//   - absolute paths (`/etc/passwd`)
//   - parent traversal (`../../foo`, even after Clean)
//   - symlinks or hardlinks (Features have no need; validating link
//     targets is fiddlier than the value buys)
//   - non-regular, non-dir types (devices, fifos, etc.)
func extractTar(r io.Reader, dst string) error {
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		clean := filepath.Clean(h.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("tar entry escapes dst: %q", h.Name)
		}
		out := filepath.Join(dstAbs, clean)
		rel, err := filepath.Rel(dstAbs, out)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("tar entry escapes dst: %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("tar entry %q is a symlink/hardlink; not supported", h.Name)
		default:
			return fmt.Errorf("tar entry %q has unsupported type %q", h.Name, string(h.Typeflag))
		}
	}
}
