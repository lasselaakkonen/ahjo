package devcontainer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFeatureRef_Tag(t *testing.T) {
	cases := []struct {
		in   string
		want FeatureRef
	}{
		{"ghcr.io/devcontainers/features/node:1", FeatureRef{"ghcr.io", "devcontainers/features/node", "1"}},
		{"ghcr.io/devcontainers/features/common-utils:2.1.0", FeatureRef{"ghcr.io", "devcontainers/features/common-utils", "2.1.0"}},
		{"ghcr.io/foo/bar", FeatureRef{"ghcr.io", "foo/bar", "latest"}}, // default tag
		{"localhost:5000/x/y:dev", FeatureRef{"localhost:5000", "x/y", "dev"}},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseFeatureRef(c.in)
			if err != nil {
				t.Fatalf("parse %q: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("parse %q = %+v; want %+v", c.in, got, c.want)
			}
		})
	}
}

func TestParseFeatureRef_Digest(t *testing.T) {
	got, err := ParseFeatureRef("ghcr.io/foo/bar@sha256:abcdef")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Reference != "sha256:abcdef" {
		t.Fatalf("Reference = %q; want sha256:abcdef", got.Reference)
	}
}

func TestParseFeatureRef_Errors(t *testing.T) {
	cases := []string{
		"",                   // empty
		"foo",                // no /
		"foo/bar",            // host without . or :
		"ghcr.io/foo:",       // empty tag
		"ghcr.io/foo@sha256", // bad digest
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if _, err := ParseFeatureRef(c); err == nil {
				t.Fatalf("expected error for %q", c)
			}
		})
	}
}

func TestParseFeatureRef_StringRoundTrip(t *testing.T) {
	for _, s := range []string{
		"ghcr.io/devcontainers/features/node:1",
		"ghcr.io/foo/bar@sha256:deadbeef",
	} {
		ref, err := ParseFeatureRef(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if ref.String() != s {
			t.Fatalf("round-trip: %q → %q", s, ref.String())
		}
	}
}

func TestParseBearerChallenge(t *testing.T) {
	header := `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:devcontainers/features/node:pull"`
	realm, params, err := parseBearerChallenge(header)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if realm != "https://ghcr.io/token" {
		t.Errorf("realm = %q", realm)
	}
	if params["service"] != "ghcr.io" {
		t.Errorf("service = %q", params["service"])
	}
	if params["scope"] != "repository:devcontainers/features/node:pull" {
		t.Errorf("scope = %q", params["scope"])
	}
}

func TestParseBearerChallenge_RejectsNonBearer(t *testing.T) {
	if _, _, err := parseBearerChallenge(`Basic realm="x"`); err == nil {
		t.Fatal("expected error rejecting Basic challenge")
	}
}

func TestSplitChallengeParams_QuoteAware(t *testing.T) {
	got := splitChallengeParams(`realm="x,y", scope="a,b"`)
	if len(got) != 2 || got[0] != `realm="x,y"` || got[1] != `scope="a,b"` {
		t.Fatalf("got %v", got)
	}
}

func TestExtractTar_HappyPath(t *testing.T) {
	dst := t.TempDir()
	buf := buildTar(t, []tarEntry{
		{Name: "./", Type: tar.TypeDir, Mode: 0o755},
		{Name: "./install.sh", Type: tar.TypeReg, Mode: 0o755, Body: []byte("#!/bin/bash\n")},
		{Name: "./devcontainer-feature.json", Type: tar.TypeReg, Mode: 0o644, Body: []byte("{}")},
	})
	if err := extractTar(buf, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, name := range []string{"install.sh", "devcontainer-feature.json"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Errorf("missing %s after extract: %v", name, err)
		}
	}
	st, err := os.Stat(filepath.Join(dst, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0o111 == 0 {
		t.Errorf("install.sh not executable: %v", st.Mode())
	}
}

func TestExtractTar_RejectsEscape(t *testing.T) {
	cases := []string{
		"../escape",
		"/etc/passwd",
		"foo/../../escape",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			dst := t.TempDir()
			buf := buildTar(t, []tarEntry{
				{Name: name, Type: tar.TypeReg, Mode: 0o644, Body: []byte("x")},
			})
			err := extractTar(buf, dst)
			if err == nil {
				t.Fatalf("expected escape rejection for %q", name)
			}
			if !strings.Contains(err.Error(), "escapes dst") {
				t.Fatalf("error should mention escape; got %v", err)
			}
		})
	}
}

func TestExtractTar_RejectsSymlink(t *testing.T) {
	dst := t.TempDir()
	buf := buildTar(t, []tarEntry{
		{Name: "link", Type: tar.TypeSymlink, Linkname: "../escape"},
	})
	err := extractTar(buf, dst)
	if err == nil || !strings.Contains(err.Error(), "symlink/hardlink") {
		t.Fatalf("expected symlink rejection; got %v", err)
	}
}

func TestFetcher_BearerHandshakeAndExtract(t *testing.T) {
	// Synthetic OCI registry: 401 once with a Bearer challenge, then
	// hand out a token that lets the manifest + blob through.
	feature := buildTar(t, []tarEntry{
		{Name: "./install.sh", Type: tar.TypeReg, Mode: 0o755, Body: []byte("#!/bin/sh\n")},
		{Name: "./devcontainer-feature.json", Type: tar.TypeReg, Mode: 0o644, Body: []byte(`{"id":"x","version":"1"}`)},
	})
	const token = "test-token-xyz"
	const digest = "sha256:abc"

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") == "" {
			http.Error(w, "missing scope", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	}))
	defer tokenSrv.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenSrv.URL+`",service="test",scope="repository:foo/bar:pull"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/foo/bar/manifests/1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schemaVersion": 2,
				"layers": []map[string]any{
					{
						"mediaType": featureLayerMediaType,
						"digest":    digest,
						"size":      feature.Len(),
					},
				},
			})
		case "/v2/foo/bar/blobs/" + digest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.Copy(w, feature)
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()

	host := strings.TrimPrefix(registry.URL, "http://")
	// Strip the scheme via a custom transport so the fetcher's https://
	// constructed URL routes to the test server. The fetcher doesn't
	// know its target is HTTP — we rewrite at transport time.
	rewrite := &rewritingTransport{rt: http.DefaultTransport, hostHTTP: host, hostHTTPS: "registry.test"}
	client := &http.Client{Transport: rewrite}
	f := &Fetcher{HTTP: client}

	dst := t.TempDir()
	if err := f.Fetch(context.Background(), FeatureRef{"registry.test", "foo/bar", "1"}, dst); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "install.sh")); err != nil {
		t.Errorf("install.sh missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "devcontainer-feature.json")); err != nil {
		t.Errorf("metadata missing: %v", err)
	}
}

// rewritingTransport rewrites the Host of an outgoing request so the
// fetcher's https://hostHTTPS/... URLs reach a local httptest server
// at hostHTTP.
type rewritingTransport struct {
	rt        http.RoundTripper
	hostHTTP  string
	hostHTTPS string
}

func (r *rewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == r.hostHTTPS {
		req2 := req.Clone(req.Context())
		req2.URL.Scheme = "http"
		req2.URL.Host = r.hostHTTP
		req2.Host = r.hostHTTP
		return r.rt.RoundTrip(req2)
	}
	return r.rt.RoundTrip(req)
}

type tarEntry struct {
	Name     string
	Type     byte
	Mode     int64
	Body     []byte
	Linkname string
}

func buildTar(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for _, e := range entries {
		h := &tar.Header{
			Name:     e.Name,
			Mode:     e.Mode,
			Size:     int64(len(e.Body)),
			Typeflag: e.Type,
			Linkname: e.Linkname,
		}
		if e.Type == tar.TypeDir {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.Type == tar.TypeReg && len(e.Body) > 0 {
			if _, err := tw.Write(e.Body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf
}
