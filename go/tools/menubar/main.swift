// pkgcache-menubar — the macOS half of pkgcache's status bar item.
//
// It draws an NSStatusItem and a menu, and it decides nothing. State arrives on stdin as
// newline-delimited commands; a click leaves on stdout as one line. Everything about caches
// — what the labels say, which items are enabled, what a click does — lives in pkgcache
// itself, which is why this file is short and has no configuration.
//
//   in   state {"running":true,"full":false,"project":"global",…}
//        menu  [{"id":0,"label":"Open pkgcache","enabled":true},…]
//        open  http://127.0.0.1:41780/widget
//        quit
//   out  click 3
//
// It also owns the window. `pkgcache-menubar -window <url>` shows one WKWebView in an
// NSWindow and nothing else — that is `pkgcache widget` on a Mac, and it needs no browser
// installed at all. The same window opens in-process when the menu asks for it, which is
// why this is one helper and not two: WKWebView and NSStatusItem both want the main
// thread, and one process that owns it is simpler than two that share nothing.
//
// It exists because NSStatusItem is Cocoa and pkgcache is built with CGO_ENABLED=0 for five
// targets from one host. A separate signed helper keeps that true: this is the only part of
// the product that needs a Mac to build.
//
// Deliberately narrow in its API surface: NSStatusItem, NSMenu, WKWebView and seven filled
// rectangles. The mark is drawn rather than borrowed from SF Symbols because it is the
// product's own — the same brackets the wordmark, the favicon and the Linux and Windows
// trays use — and because a template image lets the system recolour it for whichever menu
// bar it lands in.
//
// Build:  swiftc -O -o pkgcache-menubar main.swift
// Sign:   codesign --options runtime --timestamp -s "$IDENTITY" pkgcache-menubar

import AppKit
import Foundation
import WebKit

struct MenuItemSpec: Decodable {
    let id: Int?
    let label: String?
    let enabled: Bool?
    let separator: Bool?
}

struct CacheState: Decodable {
    let running: Bool
    let full: Bool
    let project: String?
}

/// Window-only mode: open it once the application is up, and exit when it closes.
final class WindowOnlyDelegate: NSObject, NSApplicationDelegate {
    private let windows: WindowController
    private let url: String

    init(windows: WindowController, url: String) {
        self.windows = windows
        self.url = url
        super.init()
    }

    func applicationDidFinishLaunching(_: Notification) {
        windows.show(url, title: "pkgcache")
    }
}

/// One window with the platform's own web engine in it.
///
/// Kept alive by the controller that made it: an NSWindow with no strong reference is
/// released the moment it leaves scope, which looks exactly like a window that never opened.
final class WindowController: NSObject, NSWindowDelegate {
    private var window: NSWindow?
    private let closesApp: Bool

    init(closesApp: Bool) {
        self.closesApp = closesApp
        super.init()
    }

    func show(_ url: String, title: String) {
        if let existing = window {
            // Already open: raise it rather than stacking a second one. Somebody clicking
            // the menu twice means "show me", not "give me another".
            existing.makeKeyAndOrderFront(nil)
            NSApp.activate(ignoringOtherApps: true)
            return
        }
        guard let target = URL(string: url) else { return }
        let view = WKWebView(frame: .zero, configuration: WKWebViewConfiguration())
        let created = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 420, height: 660),
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered,
            defer: false)
        created.title = title
        created.contentView = view
        created.delegate = self
        created.center()
        created.makeKeyAndOrderFront(nil)
        window = created
        view.load(URLRequest(url: target))
        NSApp.activate(ignoringOtherApps: true)
    }

    func windowWillClose(_: Notification) {
        window = nil
        // In window-only mode the window *is* the program. With a status item the icon
        // outlives it, which is the whole point of having one.
        if closesApp {
            NSApp.terminate(nil)
        }
    }
}

final class Controller: NSObject, NSApplicationDelegate {
    private var item: NSStatusItem!
    private let menu = NSMenu()
    private var state = CacheState(running: false, full: false, project: nil)
    private let windows = WindowController(closesApp: false)

    func applicationDidFinishLaunching(_: Notification) {
        item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        item.menu = menu
        redraw()
        readLines()
    }

    /// One line per command, read off the main thread and applied on it: AppKit is not
    /// thread-safe, and a menu rebuilt from a background queue is a crash waiting for a
    /// click.
    private func readLines() {
        let handle = FileHandle.standardInput
        DispatchQueue.global(qos: .utility).async { [weak self] in
            var buffer = Data()
            while true {
                let chunk = handle.availableData
                if chunk.isEmpty { break }  // pkgcache exited; so do we.
                buffer.append(chunk)
                while let newline = buffer.firstIndex(of: 0x0a) {
                    let line = Data(buffer[buffer.startIndex..<newline])
                    buffer.removeSubrange(buffer.startIndex...newline)
                    if let text = String(data: line, encoding: .utf8) {
                        DispatchQueue.main.async { self?.apply(text) }
                    }
                }
            }
            DispatchQueue.main.async { NSApp.terminate(nil) }
        }
    }

    private func apply(_ line: String) {
        if line == "quit" {
            NSApp.terminate(nil)
            return
        }
        guard let space = line.firstIndex(of: " ") else { return }
        let verb = String(line[line.startIndex..<space])
        let payload = String(line[line.index(after: space)...])
        guard let data = payload.data(using: .utf8) else { return }
        switch verb {
        case "state":
            if let decoded = try? JSONDecoder().decode(CacheState.self, from: data) {
                state = decoded
                redraw()
            }
        case "menu":
            if let specs = try? JSONDecoder().decode([MenuItemSpec].self, from: data) {
                rebuild(specs)
            }
        case "open":
            // pkgcache asks for the window rather than opening it itself: on a Mac the
            // window is this process, and a browser is not involved at any point.
            windows.show(payload, title: "pkgcache")
        default:
            break
        }
    }

    private func rebuild(_ specs: [MenuItemSpec]) {
        menu.removeAllItems()
        for spec in specs {
            if spec.separator == true {
                menu.addItem(NSMenuItem.separator())
                continue
            }
            guard let id = spec.id, let label = spec.label else { continue }
            let entry = NSMenuItem(title: label, action: #selector(chose(_:)), keyEquivalent: "")
            entry.target = self
            entry.tag = id
            entry.isEnabled = spec.enabled ?? true
            menu.addItem(entry)
        }
    }

    @objc private func chose(_ sender: NSMenuItem) {
        // stdout is the whole return channel, and it is unbuffered here: pkgcache is
        // blocked on a read, and a buffered click is a menu that appears to do nothing.
        if let data = "click \(sender.tag)\n".data(using: .utf8) {
            FileHandle.standardOutput.write(data)
        }
    }

    /// The two colours the mark is drawn in.
    ///
    /// The product's blue while it is caching, and its red once it has stopped storing —
    /// the same two the Linux and Windows trays use, so the icon means the same thing on
    /// whichever machine somebody happens to be looking at.
    ///
    /// Both clear 3:1 against a light and a dark menu bar, which is the contrast floor for
    /// an icon: the blue measures 3.45 on light and 4.48 on dark, the red 4.45 and 3.47.
    /// Neither survives a mid-grey wallpaper showing through, but no single colour does,
    /// which is the price of colour over a template image.
    private static let caching = NSColor(
        srgbRed: 0x4A / 255.0, green: 0x80 / 255.0, blue: 0xF0 / 255.0, alpha: 1)
    private static let stopped = NSColor(
        srgbRed: 0xD0 / 255.0, green: 0x3B / 255.0, blue: 0x3B / 255.0, alpha: 1)

    /// Whether to hand the system a template image instead and let it recolour the mark.
    ///
    /// Apple's guidance is that a status item should be a template, and a menu bar kept
    /// deliberately monochrome is a real preference rather than a hypothetical one. It is
    /// off by default because the mark is a coloured mark everywhere else in this product,
    /// and a status item that alone refuses to show it looks like a different program.
    private static let monochrome =
        ProcessInfo.processInfo.environment["PKGCACHE_TRAY_MONOCHROME"] == "1"

    /// pkgcache's own mark: two brackets around a cursor block.
    ///
    /// The same shape the wordmark, the favicon and the Linux and Windows trays draw, so
    /// the product looks like one product in three status bars. Built once per colour —
    /// the shape never changes, only which of the two is shown and how opaque it is.
    private static func mark(_ color: NSColor) -> NSImage {
        let side: CGFloat = 18
        let image = NSImage(size: NSSize(width: side, height: side), flipped: false) { _ in
            // Drawn with rectangles rather than a stroked path: a stroke is centred on its
            // line, so half of it lands on each side and every edge is a half-pixel one. A
            // filled rectangle lands exactly where it is put.
            //
            // Every number here is even for the same reason. The menu bar renders this at
            // 18 points, which is 18 pixels on a plain display and 36 on a Retina one, so
            // only whole points with even thicknesses land on a pixel boundary in both. At
            // 1.6 points every stroke had one blurred edge.
            let thick: CGFloat = 2
            let inset: CGFloat = 3
            let armLength: CGFloat = 4
            let top = side - inset - thick
            let rects = [
                // Left bracket: upright, top arm, bottom arm.
                NSRect(x: inset, y: inset, width: thick, height: side - inset * 2),
                NSRect(x: inset, y: top, width: armLength, height: thick),
                NSRect(x: inset, y: inset, width: armLength, height: thick),
                // Right bracket, mirrored.
                NSRect(x: side - inset - thick, y: inset, width: thick, height: side - inset * 2),
                NSRect(x: side - inset - armLength, y: top, width: armLength, height: thick),
                NSRect(x: side - inset - armLength, y: inset, width: armLength, height: thick),
                // The cursor block between them.
                NSRect(x: side / 2 - 1, y: side / 2 - 2, width: 2, height: 4),
            ]
            // A template image is a mask: only its coverage is kept, so the colour drawn
            // here is discarded and the system supplies its own.
            (Controller.monochrome ? NSColor.black : color).setFill()
            for rect in rects {
                NSBezierPath(rect: rect).fill()
            }
            return true
        }
        image.isTemplate = Controller.monochrome
        return image
    }

    private static let cachingMark = mark(caching)
    private static let stoppedMark = mark(stopped)

    private func redraw() {
        item.button?.image = state.full ? Controller.stoppedMark : Controller.cachingMark
        // Only a template image is recoloured by the tint, and the coloured mark is not
        // one — it carries its own colour, so setting a tint here would do nothing when
        // colour is on and override the system's choice when it is off.
        item.button?.contentTintColor = nil
        // Asleep is dimmer rather than different: a second glyph would have to be learned.
        item.button?.alphaValue = state.running ? 1.0 : 0.55
        item.button?.toolTip = state.running
            ? (state.full
                ? "pkgcache — FULL: serving, not storing"
                : "pkgcache — \(state.project ?? "global")")
            : "pkgcache — asleep"
    }
}

// Two modes, chosen by the argument pkgcache passes.
//
//   -window <url>   one window and nothing else, for `pkgcache widget`. A regular
//                   activation policy, so it takes focus and appears in the Dock like the
//                   application it is.
//   (no arguments)  the status bar item, for `pkgcache tray`. An accessory: no Dock icon,
//                   no menu bar of its own, no app switcher entry.

// ---- being double-clicked ---------------------------------------------------------
//
// pkgcache drives this helper over a pair of pipes: it writes state in, reads clicks out.
// Double-clicking the app in Finder starts the same binary with no pipes and no pkgcache
// behind it, so the icon would appear and then sit there forever, blank and inert — the
// worst possible outcome, because it looks like it worked.
//
// So a helper started that way turns itself into `pkgcache tray`, which is the command a
// terminal would have run. That spawns this binary again, this time with the pipes it
// expects, and everything downstream is the path that already works. Nothing new is
// invented for the GUI case; it just arrives at the same place by a different door.

/// Whether this process was started by a person rather than by pkgcache.
///
/// The pipe is the tell. LaunchServices gives a GUI process /dev/null on stdin and a
/// terminal gives it a tty; only pkgcache gives it a pipe.
func startedWithoutPkgcache() -> Bool {
    var info = stat()
    if fstat(STDIN_FILENO, &info) != 0 {
        return true
    }
    // Cast rather than compared directly: st_mode is a mode_t and Swift imports the
    // S_IF* constants as Int32, so the bare expression does not type-check.
    return (info.st_mode & mode_t(S_IFMT)) != mode_t(S_IFIFO)
}

/// Where pkgcache is, for a process that cannot rely on PATH.
///
/// An app launched from Finder inherits a minimal PATH — /usr/bin:/bin:/usr/sbin:/sbin —
/// which does not include /usr/local/bin, where pkgcache installs. Looking only on PATH
/// would fail for the ordinary installation and succeed only from a terminal, which is
/// exactly backwards.
func locatePkgcache() -> String? {
    var candidates = ["/usr/local/bin/pkgcache", "/opt/homebrew/bin/pkgcache"]
    let environment = ProcessInfo.processInfo.environment
    if let home = environment["HOME"] {
        candidates.append(home + "/.local/bin/pkgcache")
    }
    for directory in (environment["PATH"] ?? "").split(separator: ":") {
        candidates.append(String(directory) + "/pkgcache")
    }
    return candidates.first { FileManager.default.isExecutableFile(atPath: $0) }
}

/// Says what went wrong, on screen, because there is no terminal to say it in.
func reportMissingPkgcache() {
    NSApplication.shared.setActivationPolicy(.regular)
    NSApplication.shared.activate(ignoringOtherApps: true)
    let alert = NSAlert()
    alert.alertStyle = .critical
    alert.messageText = "pkgcache is not installed"
    alert.informativeText = """
        The menu bar item is the front of pkgcache, and it cannot find it.

        Looked in /usr/local/bin, /opt/homebrew/bin and ~/.local/bin.

        Install pkgcache and open this again.
        """
    alert.addButton(withTitle: "OK")
    alert.runModal()
}

func becomePkgcacheTray() {
    guard let pkgcache = locatePkgcache() else {
        reportMissingPkgcache()
        exit(1)
    }
    // pkgcache is about to look for the menu bar helper, and this process is it. Telling it
    // outright removes the whole search: an app opened from Finder has a PATH of
    // /usr/bin:/bin:/usr/sbin:/sbin, which contains neither /usr/local/bin nor the inside of
    // this bundle, so the search would fail and the failure would go to a stderr nobody is
    // reading. Clicking the icon would do nothing, with no way to find out why.
    if let me = Bundle.main.executablePath ?? CommandLine.arguments.first {
        setenv("PKGCACHE_MENUBAR", me, 1)
    }
    // execv rather than spawn-and-exit: one process from launch to quit, so quitting from
    // the menu ends the thing the person started rather than orphaning a child of it.
    var arguments: [UnsafeMutablePointer<CChar>?] = [strdup(pkgcache), strdup("tray"), nil]
    defer { arguments.forEach { free($0) } }
    execv(pkgcache, &arguments)
    // execv only returns on failure.
    reportMissingPkgcache()
    exit(1)
}

let app = NSApplication.shared
let arguments = CommandLine.arguments

// Not `arguments.count == 1`: LaunchServices has historically passed a process serial
// number (-psn_0_…) to an app it opens, and any such extra argument would have sent this
// down the branch that draws an icon nothing is feeding.
if !arguments.contains("-window") && startedWithoutPkgcache() {
    becomePkgcacheTray()
}

if let index = arguments.firstIndex(of: "-window"), index + 1 < arguments.count {
    app.setActivationPolicy(.regular)
    let windows = WindowController(closesApp: true)
    let delegate = WindowOnlyDelegate(windows: windows, url: arguments[index + 1])
    app.delegate = delegate
    app.run()
} else {
    app.setActivationPolicy(.accessory)
    let controller = Controller()
    app.delegate = controller
    app.run()
}
