package main

import "golang.org/x/sys/windows"

// withoutEcho runs read with the console on fd not echoing what is typed.
func withoutEcho(fd int, read func() (string, error)) (string, error) {
	handle := windows.Handle(fd)
	var saved uint32
	if err := windows.GetConsoleMode(handle, &saved); err != nil {
		return read()
	}
	quiet := saved&^windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT
	if err := windows.SetConsoleMode(handle, quiet); err != nil {
		return read()
	}
	defer func() { _ = windows.SetConsoleMode(handle, saved) }()
	return read()
}
