package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/mirror"
)

// bootstrapPaths runs a bootstrap walk over a fixture tree and returns every
// path the watcher emitted, plus the reassembled file bodies.
func bootstrapPaths(t *testing.T, noSkiplist bool) (paths []string, bodies map[string]string) {
	t.Helper()
	src := t.TempDir()
	files := map[string]string{
		".git/config":            "secret",
		".git/hooks/pre-commit":  "x",
		"sub/.git/HEAD":          "x",
		"node_modules/pkg/index": "dep",
		".gitignore":             "ignored.txt\n",
		"ignored.txt":            "no",
		"kept.txt":               "hello",
		"dir/nested/deep.txt":    "deep",
		"sub/ok.txt":             "ok",
	}
	for rel, body := range files {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("kept.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	m, err := mirror.LoadIgnoreMatcher(src, noSkiplist)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	e := &emitter{src: src, noSkiplist: noSkiplist, m: m, fw: mirror.NewFrameWriter(&buf)}
	if err := e.bootstrapSubtree(src, true); err != nil {
		t.Fatal(err)
	}

	bodies = map[string]string{}
	fr := mirror.NewFrameReader(&buf)
	cur := ""
	for {
		ft, payload, err := fr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var msg struct {
			Path string `json:"path"`
		}
		switch ft {
		case mirror.FrameMkdir, mirror.FrameSymlink, mirror.FrameFile:
			if err := json.Unmarshal(payload, &msg); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, msg.Path)
			if ft == mirror.FrameFile {
				cur = msg.Path
				bodies[cur] = ""
			}
		case mirror.FrameData:
			bodies[cur] += string(payload)
		}
	}
	sort.Strings(paths)
	return paths, bodies
}

func TestBootstrapEmitsTreeWithoutDotGit(t *testing.T) {
	for _, noSkiplist := range []bool{false, true} {
		paths, bodies := bootstrapPaths(t, noSkiplist)
		for _, p := range paths {
			for _, c := range strings.Split(p, "/") {
				if mirror.IsDotGit(c) {
					t.Errorf("noSkiplist=%t: emitted %q", noSkiplist, p)
				}
			}
			if p == "ignored.txt" {
				t.Errorf("noSkiplist=%t: emitted a gitignored file", noSkiplist)
			}
		}
		joined := strings.Join(paths, "\n")
		for _, want := range []string{".gitignore", "kept.txt", "link", "dir", "dir/nested", "dir/nested/deep.txt", "sub/ok.txt"} {
			if !strings.Contains("\n"+joined+"\n", "\n"+want+"\n") {
				t.Errorf("noSkiplist=%t: %q not emitted; got\n%s", noSkiplist, want, joined)
			}
		}
		if has := strings.Contains(joined, "node_modules"); has != noSkiplist {
			t.Errorf("noSkiplist=%t: node_modules emitted=%t", noSkiplist, has)
		}
		if bodies["kept.txt"] != "hello" || bodies["dir/nested/deep.txt"] != "deep" {
			t.Errorf("file bodies wrong: %v", bodies)
		}
	}
}
