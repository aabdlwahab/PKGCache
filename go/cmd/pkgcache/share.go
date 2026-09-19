package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/config"
	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// `pkgcache share` is the terminal's copy of the window's switch, and the way back when
// the window is the thing that is out of reach. `off` works with no daemon running at all:
// it removes the file the next daemon would have read, so turning sharing off never
// depends on the console that was shared.

func runShare(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	port := fs.Int("port", 0, fmt.Sprintf(
		"the port other machines use (default: %d, or the one chosen before)", local.SharePort))
	fs.Usage = func() {
		shareUsage(fs.Output())
		fs.PrintDefaults()
	}
	// On or off may come before the flags as well as after them: `share on -port 8080` is
	// how people say it.
	verb := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if verb == "" && len(rest) > 0 {
		verb, rest = rest[0], rest[1:]
	}
	if len(rest) > 0 {
		return fmt.Errorf("share: unexpected argument %q", rest[0])
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return err
	}
	switch verb {
	case "":
		return shareStatus(ctx, snap)
	case "on":
		return shareOn(ctx, snap, *port)
	case "off":
		return shareOff(ctx, snap)
	default:
		return fmt.Errorf("share: %q is neither on nor off", verb)
	}
}

func shareUsage(out io.Writer) {
	_, _ = fmt.Fprintf(out, `pkgcache share — open this cache's console to other machines

usage:
  pkgcache share               whether it is shared, and at what address
  pkgcache share on            share it, behind a password you choose
  pkgcache share on            again, to change the password
  pkgcache share off           stop sharing it

This machine keeps using the cache on 127.0.0.1, exactly as before. Sharing adds a
second address, on port %d of every network this machine is on, that serves the
console and nothing else: packages and the apt proxy stay this machine's own.

Anyone with the password can do anything the console can, including deleting the cache
and moving it. The address is plain HTTP, so the password crosses the network readable
to anyone watching it; share on networks you trust. Changing the password signs
everybody out.

The password is read from the terminal, or from standard input when that is not one.

flags:
`, local.SharePort)
}

func shareStatus(ctx context.Context, snap *config.Snapshot) error {
	state, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, NoStart: true})
	if errors.Is(err, local.ErrNoDaemon) {
		port, shared, err := local.SharedPort(snap.DataDir)
		if err != nil {
			return err
		}
		if !shared {
			fmt.Println("not shared: only this machine can reach the console")
			return nil
		}
		fmt.Printf("shared on port %d, from when the cache next starts\n", port)
		return nil
	}
	if err != nil {
		return err
	}
	answer, err := local.ReadSharing(ctx, state)
	if err != nil {
		return err
	}
	printSharing(answer)
	return nil
}

func shareOn(ctx context.Context, snap *config.Snapshot, port int) error {
	password, err := readSharePassword()
	if err != nil {
		return err
	}
	// The daemon refuses it too; saying so here spares starting one to hear it.
	if len([]rune(password)) < local.MinSharePassword {
		return fmt.Errorf("share: the password has to be at least %d characters",
			local.MinSharePassword)
	}
	state, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, Notes: os.Stderr})
	if err != nil {
		return err
	}
	answer, err := local.StartSharing(ctx, state, password, port)
	if err != nil {
		return err
	}
	printSharing(answer)
	return nil
}

func shareOff(ctx context.Context, snap *config.Snapshot) error {
	state, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, NoStart: true})
	switch {
	case errors.Is(err, local.ErrNoDaemon):
		removed, err := local.Unshare(snap.DataDir)
		if err != nil {
			return err
		}
		if removed {
			fmt.Println("no longer shared")
		} else {
			fmt.Println("was not shared")
		}
		return nil
	case err != nil:
		return err
	}
	answer, err := local.StopSharing(ctx, state)
	if err != nil {
		return err
	}
	printSharing(answer)
	return nil
}

func printSharing(state controlapi.ShareState) {
	switch {
	case !state.Enabled:
		fmt.Println("not shared: only this machine can reach the console")
	case state.Error != "":
		fmt.Printf("meant to be shared, but not reachable: %s\n", state.Error)
	case len(state.URLs) == 0:
		fmt.Printf("shared on port %d, but this machine has no network address right now\n",
			state.Port)
	default:
		fmt.Println("shared: other machines open the console at")
		for _, url := range state.URLs {
			fmt.Printf("  %s/console\n", url)
		}
	}
}

// readSharePassword asks twice at a terminal, since nothing typed is shown, and once from
// anything else.
func readSharePassword() (string, error) {
	stdin := bufio.NewReader(os.Stdin)
	line := func() (string, error) {
		text, err := stdin.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || text == "") {
			return "", fmt.Errorf("share: read the password: %w", err)
		}
		return strings.TrimRight(text, "\r\n"), nil
	}
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return line()
	}
	ask := func(prompt string) (string, error) {
		fmt.Fprint(os.Stderr, prompt)
		answer, err := withoutEcho(int(os.Stdin.Fd()), line)
		fmt.Fprintln(os.Stderr)
		return answer, err
	}
	first, err := ask(fmt.Sprintf("password for the shared console (%d characters or more): ",
		local.MinSharePassword))
	if err != nil {
		return "", err
	}
	second, err := ask("the same again: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("share: the two passwords were different; nothing changed")
	}
	return first, nil
}
