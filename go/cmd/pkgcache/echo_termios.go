//go:build linux || darwin

package main

import "golang.org/x/sys/unix"

// withoutEcho runs read with the terminal on fd not echoing what is typed.
func withoutEcho(fd int, read func() (string, error)) (string, error) {
	saved, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return read()
	}
	quiet := *saved
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return read()
	}
	defer func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, saved) }()
	return read()
}
