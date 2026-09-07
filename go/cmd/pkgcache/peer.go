package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// `pkgcache peer` — borrowing bytes from the machine next to you.
//
// `setup` points this cache at a pkgreg: a server, over TLS, with a CA fingerprint
// somebody read out to you, serving whole ecosystems through a chain. This is the other
// shape — two laptops, no server, no certificate — and it is deliberately a different
// verb rather than a flag on setup, because the trust story is not the same one.
//
// A peer is asked for a digest and answers with bytes that hash to it, so neither
// machine has to believe anything the other says about names. That is what makes this
// safe to do between two developers' machines, and it is also its limit: a peer can fill
// in a file, never tell you which file to want.
//
// One command on each side, and the second one does not need the first's output: `add`
// asks the sibling for its own token. That works because a cache with no accounts allows
// the control plane to whoever can reach it, which is the same fact `pkgcache project
// create` already relies on. Where it is not true — a cache with accounts — the error
// says to run `peer token` over there and pass it.

func runPeer(ctx context.Context, args []string) error {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
		args = args[1:]
	}
	switch verb {
	case "", "ls", "list":
		return peerList(ctx, args)
	case "add", "use":
		return peerAdd(ctx, args)
	case "rm", "remove", "forget":
		return peerRemove(ctx, args)
	case "token":
		return peerToken(ctx, args)
	case "-h", "--help", "help":
		peerUsage(os.Stderr)
		return nil
	default:
		peerUsage(os.Stderr)
		return fmt.Errorf("peer: unknown subcommand %q", verb)
	}
}

func peerUsage(out *os.File) {
	_, _ = fmt.Fprint(out, `pkgcache peer — other machines' caches, as a source for this one

usage:
  pkgcache peer ls                    the siblings this project borrows from
  pkgcache peer add <address>         borrow from that cache
  pkgcache peer rm <address|name>     stop borrowing from it
  pkgcache peer token                 issue a token for a sibling that cannot ask

A peer is another pkgcache. It is asked for content by digest and answers with bytes
that hash to it, so neither machine has to trust what the other calls anything — which
is why this needs no certificate and pkgcache setup does.

The engine asks a peer before it gives up offline and before it reaches the internet, so
two machines on a plane fill each other in. What a peer cannot answer is a question about
a name: it is asked for a digest, so this cache still has to know which file it wants.
That means an index has to have been fetched here already, and only pypi and oci work
this way — the rest either do not hash their content up front, or derive their upstream
from the request and have no row for a peer to sit in.

add asks the sibling for its own token, so there is nothing to copy between machines. A
cache listens on loopback unless it was told otherwise, so the sibling has to have been
started with PKGCACHE_ADDR=0.0.0.0:41780 for anything else to reach it.

flags:
`)
}

func peerFlags(name string, args []string, wantArg bool) (*config.Snapshot, string, string, error) {
	fs := flag.NewFlagSet("peer "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	project := fs.String("project", "",
		"the project that borrows (default: the one this cache is working in)")
	fs.Usage = func() {
		peerUsage(os.Stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, "", "", err
	}
	rest := fs.Args()
	switch {
	case wantArg && len(rest) == 0:
		return nil, "", "", fmt.Errorf(
			"peer %s: which cache? `pkgcache peer %s <address>`", name, name)
	case wantArg && len(rest) > 1, !wantArg && len(rest) > 0:
		return nil, "", "", fmt.Errorf(
			"peer %s: unexpected argument %q; flags come before the address",
			name, rest[len(rest)-1])
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return nil, "", "", err
	}
	scope := *project
	if scope == "" {
		scope = local.CurrentProject(snap.DataDir)
	}
	argument := ""
	if wantArg {
		argument = rest[0]
	}
	return snap, scope, argument, nil
}

func peerList(ctx context.Context, args []string) error {
	snap, project, _, err := peerFlags("ls", args, false)
	if err != nil {
		return err
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	peers, err := local.ListPeers(ctx, state, project)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		fmt.Printf("pkgcache: %s borrows from nobody\n", project)
		fmt.Println("  `pkgcache peer add <address>` to point it at another machine's cache")
		return nil
	}
	// One line per sibling rather than per row: three rows are one machine, and
	// printing it three times reads as three machines.
	shown := map[string]bool{}
	for _, peer := range peers {
		if shown[peer.Name] {
			continue
		}
		shown[peer.Name] = true
		var ecos []string
		seen := map[string]bool{}
		for _, row := range peers {
			if row.Name == peer.Name && !seen[row.Eco] {
				seen[row.Eco] = true
				ecos = append(ecos, row.Eco)
			}
		}
		fmt.Printf("%-20s %s  (%s)\n", peer.Name, peer.URL, strings.Join(ecos, ", "))
	}
	return nil
}

func peerAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("peer add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	project := fs.String("project", "", "the project that borrows")
	name := fs.String("name", "", "what to call it here (default: its host)")
	token := fs.String("token", "",
		"a peer token from that cache; without one, it is asked for its own")
	fs.Usage = func() { peerUsage(os.Stderr); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("peer add: which cache? `pkgcache peer add <address>`")
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return err
	}
	scope := *project
	if scope == "" {
		scope = local.CurrentProject(snap.DataDir)
	}
	address, err := local.NormalizePeerURL(fs.Arg(0))
	if err != nil {
		return err
	}
	// Reached before anything is written, so a typo costs a message rather than a set of
	// rows pointing at nothing.
	if err := local.ReachPeer(ctx, address); err != nil {
		return err
	}
	secret := *token
	if secret == "" {
		here, hostErr := os.Hostname()
		if hostErr != nil || here == "" {
			here = "a sibling"
		}
		secret, err = local.MintPeerToken(ctx, address, here)
		if err != nil {
			return fmt.Errorf("%s would not issue a token: %w\n"+
				"  run `pkgcache peer token` on that machine and pass it with -token",
				address, err)
		}
	}
	label := *name
	if label == "" {
		label = local.PeerName(address)
	}

	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	added, err := local.AddPeer(ctx, state, scope, label, address, secret)
	if err != nil {
		return err
	}
	var ecos []string
	for _, row := range added {
		ecos = append(ecos, row.Eco)
	}
	fmt.Printf("pkgcache: %s borrows from %s (%s)\n", scope, address, strings.Join(ecos, ", "))
	fmt.Println("  asked before this cache gives up offline, and before it reaches the internet")
	fmt.Println("  it answers by digest, so an index still has to be fetched here first")
	return nil
}

func peerRemove(ctx context.Context, args []string) error {
	snap, project, target, err := peerFlags("rm", args, true)
	if err != nil {
		return err
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	removed, err := local.RemovePeer(ctx, state, project, target)
	if err != nil {
		return err
	}
	fmt.Printf("pkgcache: %s no longer borrows from %s (%d row(s))\n", project, target, removed)
	return nil
}

func peerToken(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("peer token", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	label := fs.String("label", "sibling", "what this token is for, so it can be recognised later")
	fs.Usage = func() { peerUsage(os.Stderr); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return err
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return err
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	secret, err := local.MintPeerToken(ctx, state.BaseURL(), *label)
	if err != nil {
		return err
	}
	// The only time it is printed. The control plane seals it, so a second copy cannot
	// be read back out of this cache later.
	fmt.Println(secret)
	fmt.Fprintln(os.Stderr,
		"\npkgcache: give that to the other machine:\n"+
			"  pkgcache peer add <this machine's address> -token <the line above>\n"+
			"  it is shown once; this cache keeps only a hash of it")
	return nil
}
