// Package ttysecret reads a secret from a TTY with per-byte `*` echo so long
// pastes don't look like a hang. term.ReadPassword echoes nothing; this puts
// the TTY in raw mode, prints `*` per accepted byte, and handles
// Backspace/DEL, Ctrl-U (clear), Ctrl-C, and Ctrl-D.
package ttysecret

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// Read reads a line from in, echoing `*` per byte to out. The caller is
// expected to have already printed the prompt and to print a trailing newline
// after Read returns. Returns the raw (untrimmed) bytes typed before
// CR/LF/EOF.
//
// If in is not a TTY, falls back to term.ReadPassword for a silent read so the
// secret never lands in scrollback under cooked mode.
func Read(in *os.File, out io.Writer) (string, error) {
	fd := int(in.Fd())
	prev, err := term.MakeRaw(fd)
	if err != nil {
		b, perr := term.ReadPassword(fd)
		if perr != nil {
			return "", perr
		}
		return string(b), nil
	}
	defer func() { _ = term.Restore(fd, prev) }()

	var buf []byte
	one := make([]byte, 1)
	for {
		n, err := in.Read(one)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
		if n == 0 {
			continue
		}
		c := one[0]
		switch c {
		case '\r', '\n':
			return string(buf), nil
		case 0x03:
			return "", errors.New("interrupted")
		case 0x04:
			if len(buf) == 0 {
				return "", io.EOF
			}
			return string(buf), nil
		case 0x15:
			for range buf {
				fmt.Fprint(out, "\b \b")
			}
			buf = buf[:0]
		case 0x7f, 0x08:
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Fprint(out, "\b \b")
			}
		default:
			if c < 0x20 {
				continue
			}
			buf = append(buf, c)
			fmt.Fprint(out, "*")
		}
	}
	return string(buf), nil
}
