package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestQuietPreservesErrorsAndCandidateOutput(t *testing.T) {
	var stderr, stdout bytes.Buffer
	progress := terminalProgress(&stderr, true, true, false)
	fmt.Fprintln(progress, "ordinary scan progress")
	fmt.Fprintln(&stderr, "Stopped: authentication failed")
	fmt.Fprintln(&stdout, "abc")
	if got := stderr.String(); got != "Stopped: authentication failed\n" {
		t.Fatalf("quiet mode hid errors or leaked progress/banner: %q", got)
	}
	if got := stdout.String(); got != "abc\n" {
		t.Fatalf("candidate output changed: %q", got)
	}
}

func TestRedirectedProgressHasNoBannerOrANSI(t *testing.T) {
	var stderr bytes.Buffer
	progress := terminalProgress(&stderr, false, false, false)
	fmt.Fprintln(progress, "100 checks saved")
	if got := stderr.String(); got != "100 checks saved\n" {
		t.Fatalf("redirected logs contain presentation output: %q", got)
	}
}

func TestTerminalColorControls(t *testing.T) {
	previous, hadPrevious := os.LookupEnv("NO_COLOR")
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadPrevious {
			os.Setenv("NO_COLOR", previous)
		} else {
			os.Unsetenv("NO_COLOR")
		}
	})
	var colored bytes.Buffer
	terminalProgress(&colored, true, false, false)
	if !strings.Contains(colored.String(), "hunt") || !strings.Contains(colored.String(), "\x1b[") || !strings.Contains(colored.String(), "\x1b[0m") {
		t.Fatalf("interactive header missing brand or color reset: %q", colored.String())
	}
	var flagDisabled bytes.Buffer
	terminalProgress(&flagDisabled, true, false, true)
	if !strings.Contains(flagDisabled.String(), "hunt") || strings.Contains(flagDisabled.String(), "\x1b") {
		t.Fatalf("-no-color was ignored: %q", flagDisabled.String())
	}
	// Presence of NO_COLOR disables ANSI even when its value is empty.
	if err := os.Setenv("NO_COLOR", ""); err != nil {
		t.Fatal(err)
	}
	var envDisabled bytes.Buffer
	terminalProgress(&envDisabled, true, false, false)
	if !strings.Contains(envDisabled.String(), "hunt") || strings.Contains(envDisabled.String(), "\x1b") {
		t.Fatalf("NO_COLOR was ignored: %q", envDisabled.String())
	}
}
