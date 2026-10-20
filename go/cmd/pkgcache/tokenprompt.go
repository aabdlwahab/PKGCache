package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// readTokenPrompt is `-token-file -`: the team token asked for rather than read from a file.
//
// The console shows a new token once, next to the setup command that takes it, and pasting
// it at a prompt keeps it out of the shell history and the process list the way a file
// does, without leaving a file behind to delete. At a terminal nothing pasted is shown;
// from a pipe — `secret-tool lookup ... | pkgcache setup ... -token-file -` — one line is read.
func readTokenPrompt(server string) (string, error) {
	stdin := bufio.NewReader(os.Stdin)
	line := func() (string, error) {
		text, err := stdin.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || text == "") {
			return "", fmt.Errorf("setup: read the token: %w", err)
		}
		return strings.TrimSpace(text), nil
	}
	var token string
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		if token, err = line(); err != nil {
			return "", err
		}
	} else {
		fmt.Fprintf(os.Stderr, "token for %s (it is not shown as you paste it): ", server)
		token, err = withoutEcho(int(os.Stdin.Fd()), line)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
	}
	if token == "" {
		return "", errors.New("setup: no token was given")
	}
	return token, nil
}
