package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Repair a torn final write, never corruption in committed complete lines.
// The visitor performs schema validation; valid JSON without a newline survives.
func readJournal(file *os.File, visit func([]byte, int) error) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReader(file)
	var offset int64
	lineNo := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		lineNo++
		if !json.Valid(line) {
			if readErr != io.EOF || offset == 0 {
				return fmt.Errorf("corrupt JSONL at line %d", lineNo)
			}
			if err := file.Truncate(offset); err != nil {
				return err
			}
			break
		}
		if err := visit(line, lineNo); err != nil {
			return fmt.Errorf("journal line %d: %w", lineNo, err)
		}
		offset += int64(len(line))
		if readErr == io.EOF {
			if _, err := file.Seek(0, io.SeekEnd); err != nil {
				return err
			}
			if _, err := file.WriteString("\n"); err != nil {
				return err
			}
			break
		}
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	return file.Sync()
}
