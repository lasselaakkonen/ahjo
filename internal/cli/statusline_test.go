package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMergeStatusLineSetting(t *testing.T) {
	t.Run("injects when absent and preserves existing keys", func(t *testing.T) {
		merged, changed, err := mergeStatusLineSetting([]byte(`{"model":"opus","theme":"dark"}`))
		if err != nil || !changed {
			t.Fatalf("want changed, no err; got changed=%v err=%v", changed, err)
		}
		var d map[string]any
		if err := json.Unmarshal(merged, &d); err != nil {
			t.Fatalf("merged output is not valid json: %v", err)
		}
		if d["model"] != "opus" || d["theme"] != "dark" {
			t.Errorf("existing keys not preserved: %v", d)
		}
		sl, ok := d["statusLine"].(map[string]any)
		if !ok {
			t.Fatalf("statusLine not an object: %v", d["statusLine"])
		}
		if sl["type"] != "command" || sl["command"] != ahjoStatuslinePath {
			t.Errorf("statusLine wired incorrectly: %v", sl)
		}
	})

	t.Run("empty input yields a minimal settings.json", func(t *testing.T) {
		for _, raw := range [][]byte{nil, []byte(""), []byte("   \n")} {
			merged, changed, err := mergeStatusLineSetting(raw)
			if err != nil || !changed {
				t.Fatalf("empty %q: want changed, no err; got changed=%v err=%v", raw, changed, err)
			}
			if !strings.Contains(string(merged), "statusLine") {
				t.Errorf("empty %q did not produce a statusLine: %s", raw, merged)
			}
		}
	})

	t.Run("respects an existing statusLine", func(t *testing.T) {
		merged, changed, err := mergeStatusLineSetting([]byte(`{"statusLine":{"type":"command","command":"mine"}}`))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if changed || merged != nil {
			t.Errorf("user statusLine should be left alone; changed=%v merged=%q", changed, merged)
		}
	})

	t.Run("malformed json errors rather than clobbering", func(t *testing.T) {
		if _, _, err := mergeStatusLineSetting([]byte(`{not valid`)); err == nil {
			t.Error("malformed input should return an error")
		}
	})
}
