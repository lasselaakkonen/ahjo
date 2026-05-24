package ahjostate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/ports"
)

var fixedTime = time.Date(2026, 5, 24, 14, 3, 0, 0, time.UTC)

func TestRenderAllOff(t *testing.T) {
	out := RenderMarkdown(State{Slug: "acme-api-fix", Alias: "fix", At: fixedTime})

	wantContains := []string{
		"# ahjo-state",
		"_updated 2026-05-24T14:03Z · slug acme-api-fix_",
		"- **mirror**: OFF — enable from host: `ahjo mirror fix --target <dir>`",
		"- **expose**: OFF — enable from host: `ahjo expose fix <container-port>`",
		"- **forward**: OFF — enable from host: `ahjo forward fix <host-port>`",
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Errorf("all-off output missing %q\n--- got ---\n%s", w, out)
		}
	}
	// No tables when everything is off.
	if strings.Contains(out, "|---|---|") {
		t.Errorf("all-off output should have no tables:\n%s", out)
	}
}

func TestRenderMirrorOn(t *testing.T) {
	out := RenderMarkdown(State{
		Slug: "s", Alias: "a", At: fixedTime,
		MirrorOn: true, MirrorRepo: "/repo", MirrorHostTarget: "/Users/foo/bar",
	})
	for _, w := range []string{
		"- **mirror**: ON — `/repo` → host `/Users/foo/bar`",
		"(create/modify only; deletions not replicated)",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("mirror-on output missing %q\n--- got ---\n%s", w, out)
		}
	}
}

func TestRenderMirrorOnNoTarget(t *testing.T) {
	out := RenderMarkdown(State{Slug: "s", At: fixedTime, MirrorOn: true})
	if !strings.Contains(out, "- **mirror**: ON — `/repo` → host\n") {
		t.Errorf("mirror-on without target should default repo to /repo and omit target:\n%s", out)
	}
}

func TestRenderExpose(t *testing.T) {
	out := RenderMarkdown(State{
		Slug: "s", At: fixedTime,
		Expose: []ports.PortPair{{Container: 5432, Host: 10801}, {Container: 8000, Host: 10802}},
	})
	for _, w := range []string{
		"- **expose**: ON",
		"| container | host |",
		"| :5432 | 127.0.0.1:10801 |",
		"| :8000 | 127.0.0.1:10802 |",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("expose output missing %q\n--- got ---\n%s", w, out)
		}
	}
}

func TestRenderForward(t *testing.T) {
	out := RenderMarkdown(State{
		Slug: "s", At: fixedTime,
		Forward: []ports.PortPair{{Container: 9000, Host: 9000}, {Container: 9001, Host: 15555}},
	})
	for _, w := range []string{
		"- **forward**: ON",
		"| host | container |",
		"| 127.0.0.1:9000 | :9000 |",
		"| 127.0.0.1:15555 | :9001 |",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("forward output missing %q\n--- got ---\n%s", w, out)
		}
	}
}

func TestRenderAllOnNoAliasFallback(t *testing.T) {
	// Alias empty: enable hints (only shown for off features) fall back to
	// "<alias>". Here everything is on, so assert the on-renderings instead.
	out := RenderMarkdown(State{
		Slug: "s", At: fixedTime,
		MirrorOn: true, MirrorRepo: "/repo", MirrorHostTarget: "/t",
		Expose:  []ports.PortPair{{Container: 3000, Host: 10000}},
		Forward: []ports.PortPair{{Container: 8080, Host: 80}},
	})
	for _, w := range []string{"- **mirror**: ON", "- **expose**: ON", "- **forward**: ON"} {
		if !strings.Contains(out, w) {
			t.Errorf("all-on output missing %q\n--- got ---\n%s", w, out)
		}
	}
	if strings.Contains(out, "OFF") {
		t.Errorf("all-on output should contain no OFF lines:\n%s", out)
	}
}

func TestRenderOffAliasFallback(t *testing.T) {
	out := RenderMarkdown(State{Slug: "s", At: fixedTime}) // no alias
	if !strings.Contains(out, "ahjo mirror <alias> --target <dir>") {
		t.Errorf("missing-alias hint should fall back to <alias>:\n%s", out)
	}
}

// stateWire mirrors stateJSON for decoding in tests (the production type is
// unexported; this asserts the exact wire contract the statusline depends on).
type stateWire struct {
	Slug   string `json:"slug"`
	Alias  string `json:"alias"`
	Mirror struct {
		On         bool   `json:"on"`
		Repo       string `json:"repo"`
		HostTarget string `json:"host_target"`
	} `json:"mirror"`
	Expose  []ports.PortPair `json:"expose"`
	Forward []ports.PortPair `json:"forward"`
}

func decodeJSON(t *testing.T, s State) stateWire {
	t.Helper()
	b, err := RenderJSON(s)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var w stateWire
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("unmarshal RenderJSON output: %v\n%s", err, b)
	}
	return w
}

func TestRenderJSONAllOff(t *testing.T) {
	b, err := RenderJSON(State{Slug: "acme-api-fix", Alias: "fix", At: fixedTime})
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	// Empty bridges must serialize as [] (not null) so the statusline's jq is
	// uniform, and the timestamp is a parseable RFC3339 instant.
	for _, w := range []string{`"expose": []`, `"forward": []`, `"on": false`, `"updated_at": "2026-05-24T14:03:00Z"`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("all-off JSON missing %q\n--- got ---\n%s", w, b)
		}
	}

	w := decodeJSON(t, State{Slug: "acme-api-fix", Alias: "fix", At: fixedTime})
	if w.Slug != "acme-api-fix" || w.Alias != "fix" || w.Mirror.On {
		t.Errorf("all-off decoded unexpectedly: %+v", w)
	}
	if len(w.Expose) != 0 || len(w.Forward) != 0 {
		t.Errorf("all-off should have no port maps: %+v", w)
	}
}

func TestRenderJSONAllOn(t *testing.T) {
	w := decodeJSON(t, State{
		Slug: "s", Alias: "a", At: fixedTime,
		MirrorOn: true, MirrorRepo: "/repo", MirrorHostTarget: "/Users/foo/bar",
		Expose:  []ports.PortPair{{Container: 5432, Host: 10801}},
		Forward: []ports.PortPair{{Container: 9000, Host: 15555}},
	})
	if !w.Mirror.On || w.Mirror.Repo != "/repo" || w.Mirror.HostTarget != "/Users/foo/bar" {
		t.Errorf("mirror decoded unexpectedly: %+v", w.Mirror)
	}
	if len(w.Expose) != 1 || w.Expose[0] != (ports.PortPair{Container: 5432, Host: 10801}) {
		t.Errorf("expose decoded unexpectedly: %+v", w.Expose)
	}
	if len(w.Forward) != 1 || w.Forward[0] != (ports.PortPair{Container: 9000, Host: 15555}) {
		t.Errorf("forward decoded unexpectedly: %+v", w.Forward)
	}
}

func TestRenderJSONMirrorRepoDefaults(t *testing.T) {
	// Repo defaults to /repo when unset, matching RenderMarkdown.
	w := decodeJSON(t, State{Slug: "s", At: fixedTime, MirrorOn: true})
	if w.Mirror.Repo != "/repo" {
		t.Errorf("mirror repo should default to /repo, got %q", w.Mirror.Repo)
	}
}
