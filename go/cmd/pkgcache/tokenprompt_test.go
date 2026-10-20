package main

import (
	"fmt"
	"os"
	"testing"
)

func withStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = saved; _ = r.Close() })
	go func() { _, _ = fmt.Fprint(w, input); _ = w.Close() }()
}

// -token-file - takes the token from a pipe as readily as from a prompt, trimmed of the
// newline a copy brings, and refuses nothing at all rather than storing an empty token.
func TestDashTokenFileReadsOneLine(t *testing.T) {
	withStdin(t, "  id.secret  \n")
	if got, err := readToken("-", "https://team.internal:8443"); err != nil || got != "id.secret" {
		t.Fatalf("readToken = %q, %v", got, err)
	}
	withStdin(t, "\n")
	if _, err := readToken("-", "https://team.internal:8443"); err == nil {
		t.Fatal("an empty token was accepted")
	}
}
