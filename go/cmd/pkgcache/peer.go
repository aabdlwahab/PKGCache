package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/config"
	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
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
	case "projects":
		return peerProjects(ctx, args)
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
  pkgcache peer projects <address>    what projects that cache has
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
	for _, sibling := range peers {
		fmt.Printf("%-16s %s  project %s\n", sibling.Name, sibling.URL, sibling.TheirProject)
		fmt.Printf("  through  %s\n", strings.Join(sibling.Through, ", "))
		if len(sibling.Offline) > 0 {
			fmt.Printf("  offline  %s\n", strings.Join(sibling.Offline, ", "))
		}
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
	theirProject := fs.String("their-project", "",
		"the project on their side (default: their global project)")
	fs.Usage = func() { peerUsage(os.Stderr); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case fs.NArg() == 0:
		return fmt.Errorf("peer add: which cache? `pkgcache peer add <address>`")
	case fs.NArg() > 1:
		// Go stops parsing flags at the first non-flag argument, so an address followed
		// by a flag arrives here as three arguments rather than one and a setting. Said
		// plainly, because the alternative reads as "which cache?" about an address the
		// person can see they typed.
		return fmt.Errorf(
			"peer add: unexpected argument %q; flags come before the address, as in\n"+
				"  pkgcache peer add -their-project research %s",
			fs.Arg(1), fs.Arg(0))
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
	// Minting is the daemon's job, not this command's: the console and the window add a
	// sibling too, and a token obtained in only one of the three would mean the offline
	// half worked from a terminal and silently not from a window.
	label := *name
	if label == "" {
		label = local.PeerName(address)
	}

	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	added, err := local.AddPeer(ctx, state, scope, controlapi.PeerSpec{
		Address: address, TheirProject: *theirProject, Name: label, Token: *token,
	})
	if err != nil {
		return err
	}
	// Two lists because they are two different promises, and saying so is the
	// difference between "it works" and "it works until you go offline".
	fmt.Printf("pkgcache: %s fetches through %s, project %s\n",
		scope, added.URL, added.TheirProject)
	fmt.Printf("  through  %s — everything a team cache would serve\n",
		strings.Join(added.Through, ", "))
	if len(added.Offline) > 0 {
		fmt.Printf("  offline  %s — answered by digest even with this project offline\n",
			strings.Join(added.Offline, ", "))
	} else {
		fmt.Println("  offline  nothing: no token, so it cannot be asked by digest")
	}
	fmt.Println("  the public registries stay behind it, so a miss there still resolves")
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
	if err := local.RemovePeer(ctx, state, project, target); err != nil {
		return err
	}
	fmt.Printf("pkgcache: %s no longer borrows from %s\n", project, target)
	return nil
}

func peerProjects(ctx context.Context, args []string) error {
	snap, project, target, err := peerFlags("projects", args, true)
	if err != nil {
		return err
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	reachable, err := local.ReachPeerFor(ctx, state, project,
		controlapi.Probe{Address: target})
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", reachable.URL)
	if len(reachable.Projects) == 0 {
		fmt.Printf("  %s\n", reachable.Reason)
		return nil
	}
	for _, name := range reachable.Projects {
		fmt.Printf("  %s\n", name)
	}
	fmt.Printf("\n  pkgcache peer add -their-project <one of those> %s\n", target)
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
