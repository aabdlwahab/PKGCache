//go:build linux

package tray

// The icon, as pixels.
//
// IconPixmap rather than IconName: a themed name would need pkgcache to install an icon
// into the user's icon theme, which is a file outside the cache directory for the sake of
// a 22-pixel square. The pixmap travels over the bus with the item and leaves nothing
// behind.
//
// ARGB32, network byte order, as the specification requires — which is the one detail that
// silently produces a blue-tinted icon if you assume the platform's own order.

// pixmap is the (iiay) the protocol wants: width, height, and the bytes.
type pixmap struct {
	Width  int32
	Height int32
	Bytes  []byte
}

// iconSize is the tray's nominal size. Hosts scale as they like; 22 is the size most of
// them ask for and the smallest at which the bracket mark stays legible.
const iconSize = 22

// iconPixmap draws the mark: pkgcache's brackets, in the accent when it is caching and in
// the alarm colour when it has stopped.
func iconPixmap(state State) []pixmap {
	accent := [3]byte{0x4a, 0x80, 0xf0} // the product's blue
	if state.Full {
		accent = [3]byte{0xd0, 0x3b, 0x3b} // and its red
	}
	dim := byte(0xff)
	if !state.Running {
		// Asleep: the same mark, faded, rather than a different one. A second glyph would
		// have to be learned; a dimmer version of this one reads immediately.
		dim = 0x66
	}
	bytes := make([]byte, iconSize*iconSize*4)
	set := func(x, y int) {
		if x < 0 || y < 0 || x >= iconSize || y >= iconSize {
			return
		}
		i := (y*iconSize + x) * 4
		bytes[i] = dim
		bytes[i+1] = accent[0]
		bytes[i+2] = accent[1]
		bytes[i+3] = accent[2]
	}
	// Two brackets and a cursor block: the same mark as the wordmark and the favicon.
	const pad, thick = 4, 2
	for y := pad; y < iconSize-pad; y++ {
		for t := 0; t < thick; t++ {
			set(pad+t, y)
			set(iconSize-pad-1-t, y)
		}
	}
	for x := pad; x < pad+5; x++ {
		for t := 0; t < thick; t++ {
			set(x, pad+t)
			set(x, iconSize-pad-1-t)
			set(iconSize-1-x, pad+t)
			set(iconSize-1-x, iconSize-pad-1-t)
		}
	}
	for y := iconSize/2 - 2; y <= iconSize/2+1; y++ {
		for x := iconSize/2 - 1; x <= iconSize/2; x++ {
			set(x, y)
		}
	}
	return []pixmap{{Width: iconSize, Height: iconSize, Bytes: bytes}}
}
