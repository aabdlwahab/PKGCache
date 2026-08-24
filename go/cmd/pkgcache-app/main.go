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
	"time"

	_ "embed"

	"github.com/aabdlwahab/PKGCache/internal/appcore"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
	"github.com/aabdlwahab/PKGCache/internal/tray"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/icons"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// The status bar glyph, in two flat colours so the panel's own background shows through.
// A filled tile reads as a sticker stuck on the bar rather than as part of it.
var (
	//go:embed icons/tray-light.png
	trayLight []byte
	//go:embed icons/tray-dark.png
	trayDark []byte
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
	notifier := notifications.New()

	app := application.New(application.Options{
		Name:        "pkgcache",
		Description: "A package cache for this machine",
		Services:    []application.Service{application.NewService(notifier)},
		// One icon in the bar, not one per launch. Clicking a launcher twice, or the
		// login entry racing a manual start, should reach the app that is already there.
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "org.pkgreg.pkgcache.app",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				// Somebody asked for it again, which means they want to see it.
				showWindow(ctx, core)
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

	// The window is created hidden and shown on demand. -background is the login entry's
	// flag: a machine that starts this at every login should get an icon, not a window
	// across whatever the person was about to do.
	window = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "pkgcache",
		Title:  "pkgcache",
		Width:  420,
		Height: 660,
		// The console is already a complete UI served on the loopback port, so the app
		// loads it rather than carrying a second copy of the same HTML. One frontend.
		URL:              "",
		Hidden:           true,
		BackgroundColour: application.NewRGB(16, 21, 26),
	})

	systemTray := app.SystemTray.New()
	if isDarwin() {
		// A template image, so macOS tints it for the menu bar's own appearance rather
		// than leaving a flat glyph that is invisible in one of the two themes.
		systemTray.SetTemplateIcon(icons.SystrayMacTemplate)
	} else {
		systemTray.SetIcon(trayLight).SetDarkModeIcon(trayDark)
	}
	systemTray.OnClick(func() { showWindow(ctx, core) })

	// Built once and then only relabelled. Rebuilding the menu on every tick would drop
	// the one a person had open, on the two platforms where an open menu is a live object.
	menu, items := buildMenu(ctx, app, core)
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

	if !background {
		showWindow(ctx, core)
	}

	// One goroutine, and it only ever reads. Wails marshals the calls it makes back onto
	// the UI thread itself, which is the reason this can be this plain.
	go func() {
		refresh()
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for range ticker.C {
			refresh()
		}
	}()

	return app.Run()
}

// window is the single window, kept here because showWindow is reached from three places —
// a tray click, a menu item and a second launch — and threading it through all of them
// would be ceremony.
var window application.Window

// showWindow points the window at the cache and brings it forward.
//
// This is the one path allowed to start the daemon: somebody has asked to look at the
// cache, and a window that opened onto "nothing is running" when starting it takes a
// second would be a worse answer than the second.
//
// Off the UI thread, and that is the whole reason for the goroutine. Starting a cold
// daemon means opening two SQLite databases and sweeping interrupted downloads, which the
// client waits up to thirty seconds for — on the UI thread that is a frozen menu and a
// spinning cursor for however long it takes. SetURL, Show and Focus each marshal
// themselves back onto the main thread, so nothing here has to.
func showWindow(ctx context.Context, core *appcore.Core) {
	go func() {
		url, err := core.WindowURL(ctx, tray.ActionWidget)
		if err != nil {
			// Not fatal, and not silent: the address is the useful half of the answer,
			// and over SSH or in a container it is the whole of it.
			fmt.Fprintf(os.Stderr, "pkgcache-app: %v\n", err)
			url = core.FallbackURL()
		}
		window.SetURL(url)
		window.Show()
		window.Focus()
	}()
}

// buildMenu renders the tray menu from the same table the CLI's tray uses.
//
// The labels and the enabled rules come from internal/tray, so the status bar item and
// `pkgcache tray` cannot drift apart in what they offer or in what they grey out.
func buildMenu(
	ctx context.Context, app *application.App, core *appcore.Core,
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
				showWindow(ctx, core)
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
