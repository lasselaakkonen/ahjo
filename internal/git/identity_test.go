package git

import (
	"testing"
)

// fromBridgeEnv is the in-VM half of the Mac→VM identity bridge. The
// Mac shim resolves identity and sets AHJO_HOST_GIT_NAME / _EMAIL /
// _SOURCE on the relay command; this side reads them. A missing or
// empty field on either name or email must fall back to the next
// resolution source (host gitconfig, then gh) — partial values aren't
// useful because the seed step needs both halves to write a usable
// .gitconfig.
func TestFromBridgeEnv(t *testing.T) {
	cases := []struct {
		name       string
		envName    string
		envEmail   string
		envSource  string
		wantOK     bool
		wantName   string
		wantEmail  string
		wantSource string
	}{
		{
			name:    "both empty → falls through",
			envName: "", envEmail: "",
			wantOK: false,
		},
		{
			name:    "name only → falls through (partial values aren't useful)",
			envName: "Lasse", envEmail: "",
			wantOK: false,
		},
		{
			name:    "email only → falls through",
			envName: "", envEmail: "lasse@laakkonen.net",
			wantOK: false,
		},
		{
			name:    "both set, explicit source",
			envName: "Lasse", envEmail: "lasse@laakkonen.net", envSource: "host gitconfig",
			wantOK: true, wantName: "Lasse", wantEmail: "lasse@laakkonen.net", wantSource: "host gitconfig",
		},
		{
			name:    "both set, source missing → labeled 'Mac host'",
			envName: "Lasse", envEmail: "lasse@laakkonen.net",
			wantOK: true, wantName: "Lasse", wantEmail: "lasse@laakkonen.net", wantSource: "Mac host",
		},
		{
			name:    "whitespace-only counts as empty",
			envName: "   ", envEmail: "   ",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envHostName, tc.envName)
			t.Setenv(envHostEmail, tc.envEmail)
			t.Setenv(envHostSource, tc.envSource)
			id, ok := fromBridgeEnv()
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v (id=%+v)", ok, tc.wantOK, id)
			}
			if !tc.wantOK {
				return
			}
			if id.Name != tc.wantName || id.Email != tc.wantEmail || id.Source != tc.wantSource {
				t.Fatalf("got %+v, want {Name:%q Email:%q Source:%q}",
					id, tc.wantName, tc.wantEmail, tc.wantSource)
			}
		})
	}
}
