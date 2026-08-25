package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/local"
)

func isDarwin() bool { return runtime.GOOS == "darwin" }

// notificationsAvailable reports whether this process can post system notifications.
//
// macOS routes them through UNUserNotificationCenter, which refuses to work without a
// bundle identifier — so a bare binary, which is what `make app` produces and what anybody
// developing this runs, cannot have them. Wails treats a service that fails to start as
// fatal, which turns "no notifications" into "the app does not launch".
//
// That trade is wrong the way round. The notification is the least important thing here:
// the tooltip carries the same fact, and a person running the binary from a terminal is
// watching its output anyway. So this is checked first and the service is simply not
// registered when it cannot work.
//
// The check is the executable's path rather than a Core Foundation call, because reading
// the real bundle identifier needs cgo in a file whose whole purpose is to avoid deciding
// anything platform-specific in Go. Inside a bundle the binary lives at
// Foo.app/Contents/MacOS/foo, and that is what the installed app is.
func notificationsAvailable() bool {
	if !isDarwin() {
		return true
	}
	executable, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(executable, ".app/Contents/MacOS/")
}

// daemonPath finds the pkgcache binary this app starts when the cache is not running.
//
// One line, because internal/local owns the answer: the app, the docker shim and anything
// else that is not pkgcache itself all need the same binary found the same way, and three
// copies of that search would eventually disagree about which one.
//
// An error rather than a fallback. local.Ensure's own default — re-execute whoever is
// calling — is silent, wrong for this binary, and costs thirty seconds of a spinning
// window before it admits anything is amiss.
func daemonPath() (string, error) {
	path, found := local.DaemonPath()
	if !found {
		return "", fmt.Errorf(
			"cannot find the pkgcache binary, which is what holds the cache.\n" +
				"  It is installed beside this app; if you are running from a build tree, put it\n" +
				"  on PATH or in this directory:  make pkgcache")
	}
	return path, nil
}

// errorHTML is the window's contents when the cache cannot be reached.
//
// The errors this shows are written to be acted on — the missing-budget one names the
// three commands that answer it — so the page's whole job is to not lose their shape.
// Rendered as preformatted text for that reason, and escaped because an error message is
// not markup.
func errorHTML(err error) string {
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<style>
  :root { color-scheme: dark light }
  body { margin:0; min-height:100vh; box-sizing:border-box; padding:28px 24px;
         background:#10151a; color:#c6d0da;
         font:13px/1.6 ui-sans-serif,system-ui,-apple-system,sans-serif }
  @media (prefers-color-scheme: light) { body { background:#fff; color:#25303a } }
  h1 { font-size:14px; margin:0 0 4px; color:#45d98a; letter-spacing:.02em }
  p  { margin:0 0 16px; color:#8b98a5 }
  pre { margin:0; padding:14px 16px; border-radius:8px; white-space:pre-wrap;
        background:#0b0f13; border:1px solid #2b3540; color:#c6d0da;
        font:12px/1.65 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace }
  @media (prefers-color-scheme: light) {
    pre { background:#f4f6f8; border-color:#d7dee5; color:#25303a }
  }
</style></head>
<body>
  <h1>pkgcache</h1>
  <p>The cache could not be started.</p>
  <pre>` + escapeHTML(err.Error()) + `</pre>
  <p style="margin-top:16px">Run that, then open this window again from the icon.</p>
</body></html>`
}

// escapeHTML is html.EscapeString, written out to keep this file's imports to the standard
// few. An error message is data, and one containing an angle bracket should not be able to
// rewrite the page it appears on.
func escapeHTML(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return replacer.Replace(s)
}
