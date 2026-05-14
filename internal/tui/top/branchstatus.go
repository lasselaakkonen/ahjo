package top

import "time"

// BranchStatus captures the per-branch git working-tree state and any
// associated GitHub PR. Built by Deps.LoadBranchStatus on a slug; rendered
// by FormatGitStatus/FormatPRStatus in the right pane. The PR pointer
// being nil with PRChecked=true means "no PR for this head branch".
//
// Error fields are strings rather than `error` so the struct round-trips
// cleanly over JSON between the Mac shim and the in-VM RPC subcommand.
type BranchStatus struct {
	GitChecked bool   `json:"git_checked"`
	Dirty      bool   `json:"dirty,omitempty"`
	DirtyFiles int    `json:"dirty_files,omitempty"`
	Ahead      int    `json:"ahead,omitempty"`
	Behind     int    `json:"behind,omitempty"`
	HeadBranch string `json:"head_branch,omitempty"`
	GitErr     string `json:"git_err,omitempty"`

	PRChecked bool      `json:"pr_checked"`
	PR        *PRStatus `json:"pr,omitempty"`
	PRErr     string    `json:"pr_err,omitempty"`

	FetchedAt time.Time `json:"fetched_at"`
}

// PRStatus is the GitHub-side half of BranchStatus.
type PRStatus struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Title  string `json:"title"`
}
