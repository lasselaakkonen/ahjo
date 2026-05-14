package cli

import "testing"

func TestSummarizeChecks(t *testing.T) {
	cases := []struct {
		name string
		in   []checkEntry
		want string
	}{
		{"empty", nil, ""},
		{
			"all check runs succeeded",
			[]checkEntry{
				{Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Status: "COMPLETED", Conclusion: "SUCCESS"},
			},
			"passed",
		},
		{
			"one check still running",
			[]checkEntry{
				{Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Status: "IN_PROGRESS"},
			},
			"checking",
		},
		{
			"queued counts as pending",
			[]checkEntry{
				{Status: "QUEUED"},
			},
			"checking",
		},
		{
			"failure wins over pending",
			[]checkEntry{
				{Status: "IN_PROGRESS"},
				{Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			"failed",
		},
		{
			"cancelled counts as failed",
			[]checkEntry{
				{Status: "COMPLETED", Conclusion: "CANCELLED"},
			},
			"failed",
		},
		{
			"neutral conclusion is benign",
			[]checkEntry{
				{Status: "COMPLETED", Conclusion: "NEUTRAL"},
				{Status: "COMPLETED", Conclusion: "SKIPPED"},
			},
			"passed",
		},
		{
			"status context: error → failed",
			[]checkEntry{
				{State: "ERROR"},
			},
			"failed",
		},
		{
			"status context: pending",
			[]checkEntry{
				{State: "PENDING"},
				{State: "SUCCESS"},
			},
			"checking",
		},
		{
			"status context: all success",
			[]checkEntry{
				{State: "SUCCESS"},
			},
			"passed",
		},
		{
			"mixed shapes: failed CheckRun beats pending StatusContext",
			[]checkEntry{
				{State: "PENDING"},
				{Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			"failed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := summarizeChecks(c.in); got != c.want {
				t.Fatalf("summarizeChecks(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
