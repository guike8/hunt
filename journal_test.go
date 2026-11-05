package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestJournalLockRejectsConcurrentWriterAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.jsonl")
	unlock, err := lockJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockJournal(path); err == nil {
		second()
		unlock()
		t.Fatal("concurrent journal writer accepted")
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal("unlock was not idempotent:", err)
	}
	again, err := lockJournal(path)
	if err != nil {
		t.Fatal("lock was not released:", err)
	}
	again()
}

func TestJournalLockChild(t *testing.T) {
	path := os.Getenv("GITHUB_FINDER_LOCK_TEST_PATH")
	if path == "" {
		return
	}
	if _, err := lockJournal(path); err != nil {
		os.Exit(2)
	}
	// Deliberately skip the unlock callback: process exit must release ownership.
	os.Exit(0)
}

func TestJournalLockAcrossProcessesAndUnexpectedExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.jsonl")
	unlock, err := lockJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	runChild := func() error {
		command := exec.Command(executable, "-test.run=^TestJournalLockChild$")
		command.Env = append(os.Environ(), "GITHUB_FINDER_LOCK_TEST_PATH="+path)
		return command.Run()
	}
	blocked := runChild()
	if exit, ok := blocked.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
		unlock()
		t.Fatalf("second process was not blocked: %v", blocked)
	}
	unlock()
	if err := runChild(); err != nil {
		t.Fatal("child could not acquire released lock:", err)
	}
	again, err := lockJournal(path)
	if err != nil {
		t.Fatal("process exit retained its lock:", err)
	}
	again()
}

func TestReadJournalRecoversOnlyIncompleteTail(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		fail              bool
	}{
		{"complete without newline", "{\"ok\":1}", "{\"ok\":1}\n", false},
		{"torn tail", "{\"ok\":1}\n{\"unfinished\":", "{\"ok\":1}\n", false},
		{"corrupt committed line", "{\"ok\":1}\n{broken}\n{\"ok\":2}\n", "", true},
		{"unrecognized file", "an existing file without a newline", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			if err := os.WriteFile(path, []byte(tc.input), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			err = readJournal(file, func([]byte, int) error { return nil })
			if (err != nil) != tc.fail {
				t.Fatalf("unexpected recovery error: %v", err)
			}
			if tc.fail {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != tc.input {
					t.Fatalf("corrupt file was modified: %q, %v", data, err)
				}
				return
			}
			if _, err := file.WriteString("{\"ok\":3}\n"); err != nil {
				t.Fatal(err)
			}
			file.Seek(0, io.SeekStart)
			data, err := io.ReadAll(file)
			if err != nil || !bytes.Equal(data, []byte(tc.want+"{\"ok\":3}\n")) {
				t.Fatalf("journal not safely appendable: %q, %v", data, err)
			}
		})
	}
}

func TestDiscoveryDoesNotTruncateUnrecognizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.txt")
	original := []byte("an existing file without a newline")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config{Version: 1, Length: 3, Alphabet: "abcdefghijklmnopqrstuvwxyz", Seed: 1}
	if s, err := openStore(path, cfg); err == nil {
		s.Close()
		t.Fatal("unrecognized journal accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("unrecognized file was truncated: %q, %v", data, err)
	}
}
