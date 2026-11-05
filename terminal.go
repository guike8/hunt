package main

import (
	"fmt"
	"io"
	"os"
)

func stderrIsTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Only ordinary progress uses this writer. Errors retain the caller's stderr,
// and names keep their independent stdout stream for piping and redirection.
func terminalProgress(stderr io.Writer, isTerminal, quiet, noColor bool) io.Writer {
	if quiet {
		return io.Discard
	}
	if isTerminal {
		_, colorDisabled := os.LookupEnv("NO_COLOR")
		if !noColor && !colorDisabled {
			fmt.Fprintln(stderr, "\x1b[1;38;2;45;212;191mhunt\x1b[0m  GitHub username finder")
		} else {
			fmt.Fprintln(stderr, "hunt  GitHub username finder")
		}
	}
	return stderr
}
