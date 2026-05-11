// Package initflow runs ordered, idempotent setup steps with per-step
// confirmation. Steps detect their own completion state, so a re-run of
// `ahjo init` resumes where the previous run halted.
package initflow

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

var ErrAborted = errors.New("aborted by user")

// Step is one phase of the init flow.
type Step struct {
	Title string
	// Skip returns (true, reason) if this step is already done.
	Skip func() (bool, string, error)
	// Show is a human-readable preview of what Action will do, printed
	// before the y/N prompt. Multi-line OK.
	Show string
	// Action does the work. Stdout/stderr passthrough is the runner's job.
	Action func(out io.Writer) error
	// Note is printed as a one-line caveat before Show (e.g. interactivity).
	Note string
	// Post is printed after a successful run.
	Post string
	// Halt stops the run after this step (e.g. usermod requires re-shell).
	Halt bool
}

type Runner struct {
	Yes bool      // skip prompts
	In  io.Reader // for prompting
	Out io.Writer
	Err io.Writer
}

func (r Runner) Execute(steps []Step) error {
	out := r.Out
	for _, s := range steps {
		if s.Skip != nil {
			done, reason, err := s.Skip()
			if err != nil {
				return fmt.Errorf("%s: detect: %w", s.Title, err)
			}
			if done {
				fmt.Fprintf(out, "[ok]   %s — %s\n", s.Title, reason)
				continue
			}
		}
		fmt.Fprintf(out, "[step] %s\n", s.Title)
		if s.Note != "" {
			fmt.Fprintf(out, "  note: %s\n", s.Note)
		}
		if s.Show != "" {
			for _, line := range strings.Split(strings.TrimRight(s.Show, "\n"), "\n") {
				fmt.Fprintf(out, "  > %s\n", line)
			}
		}
		if !r.Yes {
			ok, err := r.confirm()
			if err != nil {
				return err
			}
			if !ok {
				return ErrAborted
			}
		}
		if s.Action != nil {
			if err := s.Action(out); err != nil {
				return fmt.Errorf("%s: %w", s.Title, err)
			}
		}
		if s.Post != "" {
			fmt.Fprintln(out, s.Post)
		}
		if s.Halt {
			return nil
		}
	}
	return nil
}

func (r Runner) confirm() (bool, error) {
	fmt.Fprint(r.Out, "  Run? [y/N] ")
	sc := bufio.NewScanner(r.In)
	if !sc.Scan() {
		return false, sc.Err()
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes", nil
}

// RunShell invokes argv with stdio passthrough. If stdin is non-empty, it's
// piped on stdin. The first argument is the program; subsequent args are
// passed verbatim — no shell expansion.
func RunShell(out io.Writer, stdin string, argv ...string) error {
	return RunShellEnv(out, nil, stdin, argv...)
}

// RunShellEnv is RunShell with an explicit env. Pass nil to inherit the
// parent env. Use this when invoking limactl so SSH_AUTH_SOCK can be
// overridden via lima.Env().
func RunShellEnv(out io.Writer, env []string, stdin string, argv ...string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = out
	cmd.Stderr = out
	if env != nil {
		cmd.Env = env
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	} else {
		cmd.Stdin = os.Stdin
	}
	return cmd.Run()
}

// RunBash runs `bash -c <script>` with stdio passthrough; use for one-liners
// that legitimately need shell features (pipes, redirection, $expansion).
func RunBash(out io.Writer, stdin, script string) error {
	cmd := exec.Command("bash", "-c", script)
	cmd.Stdout = out
	cmd.Stderr = out
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	} else {
		cmd.Stdin = os.Stdin
	}
	return cmd.Run()
}
