package cli

import (
	"io"
	"os"
)

func cobraOut() io.Writer    { return os.Stdout }
func cobraOutErr() io.Writer { return os.Stderr }
