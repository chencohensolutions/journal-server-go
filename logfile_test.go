package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTheServiceLogStartsAgainAtItsLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, []byte("from the run before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log, err := openCappedLog(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer log.file.Close()
	line := bytes.Repeat([]byte("x"), 39)
	line = append(line, '\n')
	for range 2 {
		if _, err := log.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(path); len(data) != 100 || !bytes.HasPrefix(data, []byte("from the run before\n")) {
		t.Fatalf("at the limit: %d bytes", len(data))
	}
	if _, err := os.Stat(path + ".old"); err == nil {
		t.Fatal("moved before the limit")
	}
	// The next line doesn't fit: the 100 bytes move to .old and the file starts again with it.
	if _, err := log.Write([]byte("the next line\n")); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "the next line\n" {
		t.Errorf("the log after its limit: %q", data)
	}
	if old, _ := os.ReadFile(path + ".old"); len(old) != 100 {
		t.Errorf("the old log: %d bytes", len(old))
	}
	// What else writes to the same file (os.Stderr in the service) lands after it.
	log.file.WriteString("a panic\n")
	if data, _ := os.ReadFile(path); string(data) != "the next line\na panic\n" {
		t.Errorf("a direct write: %q", data)
	}
}
