// Command pkgcache-app is the desktop app: a window onto this machine's cache, and an
// icon in the status bar that watches it.
//
// It knows almost nothing. Everything it decides — what the cache is doing, what a menu
// item should say, whether something has changed enough to interrupt somebody — lives in
// internal/appcore, which has no toolkit in it and is tested on machines with no display.
// What is left here is the wiring: a window, a tray, a ticker, and the three platform
// mechanisms Wails hides behind one API.
//
// The rule it inherits, and the one thing here that is not negotiable: the app never keeps
// the daemon alive. The cache exits when nothing has used it for a while, and an app that
// held it up would quietly remove the idle exit that makes this polite to leave installed.
// Every poll reads state without starting anything; opening the window is the one action
// allowed to wake it, because that is somebody asking to look.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/aabdlwahab/PKGCache/internal/appcore"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
	"github.com/aabdlwahab/PKGCache/internal/tray"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// The status bar glyph, in two flat colours so the panel's own background shows through.
// A filled tile reads as a sticker stuck on the bar rather than as part of it.
//
// The dark one doubles as the macOS template image: a template is black plus alpha, which
// is exactly what it already is, and macOS then tints it for a light or dark menu bar and
// for being clicked. That is why there is no third file for it.
var (
	//go:embed icons/tray-light.png
	trayLight []byte
	//go:embed icons/tray-dark.png
	trayDark []byte
	// The application's own icon — the full logo, card and all — rather than the status
	// bar glyph. Different job, different image: this one is seen at 512px in an about
	// box, not at 22px against a menu bar.
	//go:embed icons/appicon.png
	appIcon []byte
)

// pollInterval is how often the tray re-reads the cache.
//
// Two seconds is slow enough to be free — the read is two small files and, when a daemon
// is up, one loopback request — and fast enough that clicking "Reclaim space" and watching
// the number fall feels like it worked.
const pollInterval = 2 * time.Second

func main() {
	background := flag.Bool("background", false,
		"start with no window, just the status bar icon (what the login entry uses)")
	onLogin := flag.Bool("on-login", false, "start this app when you log in")
	offLogin := flag.Bool("off-login", false, "stop starting it on login")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), `pkgcache-app — the window and status bar item for this machine's cache

usage: pkgcache-app [flags]

Opens a window onto the cache and puts an icon in the status bar. It shows what is
downloading, how much of the disk budget is used, how much is being served from here, and
says so when the cache has filled up and stopped storing.

It never keeps the cache running: when nothing has used it for a while the daemon exits as
usual and the icon says "asleep".

flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	// Refused rather than ignored. flag.Parse leaves positional arguments alone, so
	// `pkgcache-app status` used to open a window and say nothing — and somebody reaching
	// for the CLI on this binary is going to try `status`, `serve` or `setup` before they
	// try anything else. Twice, during the debugging of this very program.
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr,
			"pkgcache-app: %q is not something this program does — it is the window and the\n"+
				"  status bar icon, and it takes no commands. The cache itself is `pkgcache`:\n"+
				"      pkgcache %s\n",
			flag.Arg(0), strings.Join(flag.Args(), " "))
		os.Exit(2)
	}

	if err := run(*background, *onLogin, *offLogin); err != nil {
		fmt.Fprintf(os.Stderr, "pkgcache-app: %v\n", err)
		os.Exit(1)
	}
}

func run(background, onLogin, offLogin bool) error {
	snapshot, err := config.LoadLocal(config.LocalFlags{})
	if err != nil {
		return err
	}

	// The login entry is neither a window nor a cache operation, so it is handled before
	// anything else is built and the process exits without showing a thing.
	if onLogin || offLogin {
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate this program: %w", err)
		}
		return local.InstallAutostart(local.AutostartOptions{
			Executable: executable, Command: "app",
			Remove: offLogin, Out: os.Stdout,
		})
	}

	ctx := context.Background()
	core := appcore.New(snapshot)
	// Without this the app re-executes itself as the daemon, waits thirty seconds for a
	// cache that was never going to appear, and shows an empty window while it does.
	daemon, err := daemonPath()
	if err != nil {
		return err
	}
	core.UseDaemon(daemon)

	// Registered only where it can work. See notificationsAvailable: on macOS this needs
	// a bundle identifier, and Wails makes a service that fails to start fatal — which
	// would turn "no notifications" into "the app does not launch".
	var (
		notifier *notifications.NotificationService
		services []application.Service
	)
	if notificationsAvailable() {
		notifier = notifications.New()
		services = append(services, application.NewService(notifier))
	} else {
		fmt.Fprintln(os.Stderr,
			"pkgcache-app: no app bundle, so no notifications — the tooltip says the same things")
	}

	app = application.New(application.Options{
		Name:        "pkgcache",
		Description: "A package cache for this machine",
		Icon:        appIcon,
		Services:    services,
		// One icon in the bar, not one per launch. Clicking a launcher twice, or the
		// login entry racing a manual start, should reach the app that is already there.
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "org.pkgreg.pkgcache.app",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				// Somebody asked for it again, which means they want to see it.
				openWindow(ctx, core)
			},
		},
		Mac: application.MacOptions{
			// A real app with a Dock icon and a window, not a menu bar accessory. That is
			// a change from the Swift helper this replaces, which was LSUIElement — and it
			// is the point of the exercise: an app somebody can alt-tab to, rather than a
			// glyph that only exists while a menu is open.
			ActivationPolicy: application.ActivationPolicyRegular,
		},
	})

	systemTray := app.SystemTray.New()
	if isDarwin() {
		// A template image, so macOS tints it for the menu bar's own appearance — light
		// bar, dark bar, and the inverted state while the menu is open — rather than
		// leaving a flat glyph that is invisible in one of them.
		//
		// Ours, not Wails'. This said icons.SystrayMacTemplate for a while, which is the
		// Wails logo, and put a W in the menu bar of a program that is not Wails.
		systemTray.SetTemplateIcon(trayDark)
	} else {
		// Two flat images and the platform picks: no tinting to rely on here.
		systemTray.SetIcon(trayLight).SetDarkModeIcon(trayDark)
	}
	systemTray.OnClick(func() { openWindow(ctx, core) })

	// Built once and then only relabelled. Rebuilding the menu on every tick would drop
	// the one a person had open, on the two platforms where an open menu is a live object.
	menu, items := buildMenu(ctx, core)
	systemTray.SetMenu(menu)

	refresh := func() {
		// Read first, off the UI thread: this touches the filesystem and, when a daemon
		// is up, makes a loopback request. Neither belongs on the thread drawing the menu.
		state := core.State(ctx)

		// Then write, on the UI thread, and explicitly.
		//
		// SetTooltip marshals itself — it wraps its platform call in InvokeSync — but
		// MenuItem.SetLabel and SetEnabled do not: they reach straight into the platform
		// menu object on whatever goroutine called them. Updating a menu from this ticker
		// without InvokeSync is the flaky-tray bug the code this replaces warned about, so
		// the whole batch goes over at once rather than relying on which setter happens to
		// be safe.
		application.InvokeSync(func() {
			systemTray.SetTooltip(tray.Tooltip(state))
			for action, item := range items {
				item.SetLabel(action.Label(state))
				item.SetEnabled(action.Enabled(state))
			}
		})

		if notifier == nil {
			// Nothing to say it to, but Observe is still called: it is what records the
			// edge, and skipping it entirely would leave the next poll comparing against
			// a state from before whatever just happened.
			core.Observe(state)
			return
		}
		for _, note := range core.Observe(state) {
			// Best effort. A machine with no notification daemon — a bare window manager,
			// a container — is not a broken machine, and the tooltip still says the same
			// thing.
			_ = notifier.SendNotification(notifications.NotificationOptions{
				ID:    "pkgcache-" + note.Title,
				Title: note.Title,
				Body:  note.Body,
			})
		}
	}

	// Everything that touches the running application waits for it to be running.
	//
	// Both halves of this were bugs, and both were invisible until it was launched.
	// InvokeSync reaches globalApplication.dispatchOnMainThread, which dereferences an
	// impl that app.Run() is what creates — so a poll started before Run panics with a
	// nil pointer rather than waiting. And WebviewWindow.Show returns silently when that
	// same impl is nil, so showing the window early is not early, it simply never
	// happens: the app would have come up with no window and no explanation.
	//
	// ApplicationStarted fires once the platform application exists, which is the first
	// moment either is safe.
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted,
		func(*application.ApplicationEvent) {
			if !background {
				openWindow(ctx, core)
			}
			go func() {
				refresh()
				ticker := time.NewTicker(pollInterval)
				defer ticker.Stop()
				for range ticker.C {
					refresh()
				}
			}()
		})

	return app.Run()
}

// The one window, created the first time somebody asks for it.
//
// Created with its URL already set, and SetURL is never called — which is the whole point
// of this shape. WebviewWindow.Run assigns w.impl *before* dispatching the call that
// builds the native window, so there is a moment where impl is non-nil and nsWindow is
// not yet valid. SetURL's only guard is `impl != nil`, and on macOS it hands nsWindow
// straight to Objective-C:
//
//	NSWindow<WailsWebviewWindow>* window = webviewHost(nsWindow);
//	[window.webView loadRequest:request];
//
// which is a SIGSEGV inside cgo, on the main thread, with a traceback that points at
// Wails rather than at the caller. Handing the URL to NewWithOptions instead lets Wails
// construct the window and load the page in whatever order it knows to be safe.
var (
	windowMu sync.Mutex
	window   application.Window
	// app is the running application, kept here because the single-instance callback is
	// constructed inside the call that produces it and so cannot name the local. It is
	// only ever read after Run has started, which is the only time any of these fire.
	app *application.App
)

// openWindow shows the cache's window, building it the first time.
//
// Always on its own goroutine, and that matters in both directions: the caller is usually
// the UI thread inside a click handler, so this must not block it, and resolving the
// address may start a cold daemon that the client waits up to thirty seconds for.
//
// The lock is held across the whole sequence. Clicks that arrive during a cold start wait
// for the window they asked for rather than each building one of their own — two windows
// racing to be the window was an earlier version of this bug.
func openWindow(ctx context.Context, core *appcore.Core) {
	go func() {
		windowMu.Lock()
		defer windowMu.Unlock()

		if window != nil {
			window.Show()
			window.Focus()
			return
		}

		url, err := core.WindowURL(ctx, tray.ActionWidget)
		if err != nil {
			// Not fatal, and not silent: the address is the useful half of the answer,
			// and over SSH or in a container it is the whole of it.
			fmt.Fprintf(os.Stderr, "pkgcache-app: %v\n", err)
			// The configured address rather than a bound one, which is a guess: a daemon
			// that took an ephemeral port is not on it. Said out loud below for that
			// reason — "Load failed" in a webview names neither the address nor the
			// reason, and this is the only place that knows both.
			url = core.FallbackURL()
			fmt.Fprintf(os.Stderr,
				"pkgcache-app: falling back to %s, which may not be where the cache is\n", url)
		} else {
			fmt.Fprintf(os.Stderr, "pkgcache-app: window on %s\n", url)
		}

		created := app.Window.NewWithOptions(application.WebviewWindowOptions{
			Name:   "pkgcache",
			Title:  "pkgcache",
			Width:  420,
			Height: 660,
			// The console is already a complete UI served on the loopback port, so the
			// app loads it rather than carrying a second copy of the same HTML.
			URL:              url,
			BackgroundColour: application.NewRGB(16, 21, 26),
		})

		// Closing hides it rather than destroying it. This is a status bar application:
		// the icon stays whatever the window does, and a closed window that had been
		// destroyed would leave the variable above pointing at something Wails has torn
		// down — the next click would then be reaching into freed memory, which is the
		// same class of fault as the one this shape exists to avoid.
		created.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
			created.Hide()
			e.Cancel()
		})

		window = created
		window.Show()
		window.Focus()
	}()
}

// buildMenu renders the tray menu from the same table the CLI's tray uses.
//
// The labels and the enabled rules come from internal/tray, so the status bar item and
// `pkgcache tray` cannot drift apart in what they offer or in what they grey out.
func buildMenu(
	ctx context.Context, core *appcore.Core,
) (*application.Menu, map[tray.Action]*application.MenuItem) {
	menu := application.NewMenu()
	items := make(map[tray.Action]*application.MenuItem, len(tray.Menu))
	separators := tray.Separators()

	for _, action := range tray.Menu {
		if separators[action] {
			menu.AddSeparator()
		}
		choice := action
		item := menu.Add(choice.Label(tray.State{}))
		item.OnClick(func(*application.Context) {
			switch choice {
			case tray.ActionWidget, tray.ActionConsole:
				openWindow(ctx, core)
			case tray.ActionQuit:
				// Quits the icon, not the cache. The daemon has its own idle exit and
				// stopping it here would surprise whatever is mid-install.
				app.Quit()
			default:
				if err := core.Do(ctx, choice); err != nil {
					fmt.Fprintf(os.Stderr, "pkgcache-app: %v\n", err)
				}
			}
		})
		items[choice] = item
	}
	return menu, items
}
