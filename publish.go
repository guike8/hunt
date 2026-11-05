package main

import (
	"io"
	"os"
)

// Make regular-file output durable before acknowledging it in the result
// journal. Pipes and terminals are accepted by their writer without fsync.
func publishResult(out io.Writer, name string) error {
	line := name + "\n"
	written, err := io.WriteString(out, line)
	if err != nil {
		return err
	}
	if written != len(line) {
		return io.ErrShortWrite
	}
	if file, ok := out.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return file.Sync()
		}
		return nil
	}
	if stable, ok := out.(interface{ Sync() error }); ok {
		return stable.Sync()
	}
	return nil
}
