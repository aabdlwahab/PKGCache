//go:build linux && cgo && webkitgtk

package main

// WebKitGTK, which is the web engine Ubuntu already has.
//
// This is the one file in the product that needs cgo, and it is why the window is a separate
// binary: pkgcache itself stays CGO_ENABLED=0 on five targets from one host, and nothing
// about that changes to gain a window.
//
// Specialised to Ubuntu, as asked. The pkg-config name is webkit2gtk-4.1, which is what
// 24.04 ships — 24.04 dropped the 4.0 package entirely, which is also why this is written
// directly rather than through a binding: the well-known one pins 4.0 and does not build on
// the current LTS.
//
//	sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev
//	go build -tags webkitgtk -o pkgcache-window ./cmd/pkgcache-window
//
// The tag is deliberate. Without it this file is not compiled at all, so `go build ./...`
// keeps working on a machine with no GTK headers — which includes CI and every host that
// only ever builds the client.

/*
#cgo pkg-config: gtk+-3.0 webkit2gtk-4.1
#include <stdlib.h>
#include <gtk/gtk.h>
#include <webkit2/webkit2.h>

static void pkgcache_window_closed(GtkWidget *widget, gpointer data) {
	gtk_main_quit();
}

// Built in C rather than assembled call by call from Go, because g_signal_connect and the
// GTK_WINDOW / WEBKIT_WEB_VIEW casts are macros: they exist in the preprocessor and cgo
// cannot see them. One C function keeps them where they work.
static void pkgcache_window_open(const char *title, int width, int height, const char *url) {
	GtkWidget *window = gtk_window_new(GTK_WINDOW_TOPLEVEL);
	gtk_window_set_title(GTK_WINDOW(window), title);
	gtk_window_set_default_size(GTK_WINDOW(window), width, height);
	gtk_window_set_position(GTK_WINDOW(window), GTK_WIN_POS_CENTER);

	GtkWidget *view = webkit_web_view_new();
	gtk_container_add(GTK_CONTAINER(window), view);
	webkit_web_view_load_uri(WEBKIT_WEB_VIEW(view), url);

	g_signal_connect(window, "destroy", G_CALLBACK(pkgcache_window_closed), NULL);
	gtk_widget_show_all(window);
	gtk_widget_grab_focus(view);
}
*/
import "C"

import (
	"errors"
	"runtime"
	"unsafe"
)

func show(url, title string, width, height int) error {
	// GTK is not thread-safe and its main loop belongs to the thread that initialised it.
	// Without this Go is free to move the goroutine and the window stops responding.
	runtime.LockOSThread()

	// init_check rather than init: gtk_init aborts the process when there is no display,
	// which over SSH would be a crash where an explanation belongs.
	if C.gtk_init_check(nil, nil) == 0 {
		return errors.New(
			"no display to open a window on — over SSH or in a container there is none.\n" +
				"  The page is served on the address pkgcache printed; open it from a machine\n" +
				"  that has a screen")
	}

	cTitle, cURL := C.CString(title), C.CString(url)
	defer C.free(unsafe.Pointer(cTitle))
	defer C.free(unsafe.Pointer(cURL))

	C.pkgcache_window_open(cTitle, C.int(width), C.int(height), cURL)
	C.gtk_main()
	return nil
}
