package git

import "testing"

func TestSanitizeBranchName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"JIRA-123 some ticket title here", "JIRA-123-some-ticket-title-here"},
		{"feature/foo bar", "feature/foo-bar"},
		{"  spaces  ", "spaces"},
		{"feature//double/slash", "feature/double/slash"},
		{"weird::chars??", "weird-chars"},
		{"dots..in..name", "dots.in.name"},
		{"trailing.lock", "trailing"},
		{"---leading-and-trailing---", "leading-and-trailing"},
		{"emoji 🚀 ship it", "emoji-ship-it"},
		{"//.//", ""},
		{"", ""},
		{"PascalCase_Stays", "PascalCase_Stays"},
		{"a.b.c", "a.b.c"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := SanitizeBranchName(tt.in); got != tt.want {
				t.Fatalf("SanitizeBranchName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
