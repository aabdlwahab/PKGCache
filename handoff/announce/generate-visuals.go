// Command generate-visuals rebuilds the four announcement GIFs from deterministic
// SVG frames. It deliberately uses only the Go standard library; rsvg-convert is the
// one rendering tool required at generation time and is not a pkgreg dependency.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	width  = 634
	height = 557
)

type scene func(frame, total int) string

type asset struct {
	name   string
	frames int
	delay  int
	draw   scene
}

func main() {
	out, err := outputDir()
	if err != nil {
		fatal(err)
	}
	if _, err := exec.LookPath("rsvg-convert"); err != nil {
		fatal(fmt.Errorf("rsvg-convert is required to rebuild the announcement GIFs: %w", err))
	}

	assets := []asset{
		{name: "00-pkgcache-launch.gif", frames: 18, delay: 9, draw: launchScene},
		{name: "01-one-cache.gif", frames: 18, delay: 9, draw: cacheScene},
		{name: "02-versioned.gif", frames: 18, delay: 9, draw: checkpointScene},
		{name: "03-offline.gif", frames: 24, delay: 9, draw: offlineScene},
	}
	for _, item := range assets {
		if err := render(filepath.Join(out, item.name), item); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote %s\n", filepath.Join(out, item.name))
	}
}

func outputDir() (string, error) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("locate generator source")
	}
	return filepath.Dir(source), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "generate-visuals:", err)
	os.Exit(1)
}

func render(path string, item asset) error {
	frames := make([][]byte, 0, item.frames)
	for frame := 0; frame < item.frames; frame++ {
		svg := item.draw(frame, item.frames)
		command := exec.Command("rsvg-convert", "-w", fmt.Sprint(width), "-h", fmt.Sprint(height))
		command.Stdin = strings.NewReader(svg)
		encoded, err := command.Output()
		if err != nil {
			return fmt.Errorf("render %s frame %d: %w", item.name, frame, err)
		}
		decoded, err := png.Decode(bytes.NewReader(encoded))
		if err != nil {
			return fmt.Errorf("decode %s frame %d: %w", item.name, frame, err)
		}
		frames = append(frames, quantize(decoded))
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".announce-*.gif")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := writeGIF(temporary, frames, item.delay); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// quantize maps an rsvg-rendered frame to the fixed product palette. A fixed global
// table keeps every frame stable and avoids the color shimmer caused by independent
// per-frame quantization.
func quantize(source image.Image) []byte {
	bounds := source.Bounds()
	indices := make([]byte, bounds.Dx()*bounds.Dy())
	position := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := source.At(x, y).RGBA()
			indices[position] = nearest(uint8(r>>8), uint8(g>>8), uint8(b>>8))
			position++
		}
	}
	return indices
}

func nearest(red, green, blue uint8) byte {
	bestIndex := 0
	bestDistance := int64(math.MaxInt64)
	for index, candidate := range visualPalette {
		r, g, b, _ := candidate.RGBA()
		dr := int64(int(red) - int(r>>8))
		dg := int64(int(green) - int(g>>8))
		db := int64(int(blue) - int(b>>8))
		distance := 3*dr*dr + 6*dg*dg + db*db
		if distance < bestDistance {
			bestDistance = distance
			bestIndex = index
		}
	}
	return byte(bestIndex)
}

// writeGIF emits a looping GIF89a animation with one shared 64-color table. The
// implementation is intentionally small: the distro Go toolchain used to rebuild
// these assets does not expose image/gif even though its source is installed.
func writeGIF(output *os.File, frames [][]byte, delay int) error {
	var encoded bytes.Buffer
	encoded.WriteString("GIF89a")
	writeUint16(&encoded, width)
	writeUint16(&encoded, height)
	encoded.WriteByte(0xf5) // global color table, 8-bit source depth, 64 entries
	encoded.WriteByte(0)    // background palette index
	encoded.WriteByte(0)    // square pixels
	for _, entry := range visualPalette {
		r, g, b, _ := entry.RGBA()
		encoded.WriteByte(byte(r >> 8))
		encoded.WriteByte(byte(g >> 8))
		encoded.WriteByte(byte(b >> 8))
	}
	for index := len(visualPalette); index < 64; index++ {
		encoded.Write([]byte{0, 0, 0})
	}

	// Loop forever.
	encoded.Write([]byte{0x21, 0xff, 0x0b})
	encoded.WriteString("NETSCAPE2.0")
	encoded.Write([]byte{0x03, 0x01, 0x00, 0x00, 0x00})

	for _, pixels := range frames {
		// Graphic control extension: keep the complete preceding frame until the
		// next one replaces it. Delay is in hundredths of a second.
		encoded.Write([]byte{0x21, 0xf9, 0x04, 0x00})
		writeUint16(&encoded, delay)
		encoded.Write([]byte{0x00, 0x00})

		// Full-canvas image descriptor, using the global palette.
		encoded.WriteByte(0x2c)
		writeUint16(&encoded, 0)
		writeUint16(&encoded, 0)
		writeUint16(&encoded, width)
		writeUint16(&encoded, height)
		encoded.WriteByte(0)

		const minimumCodeSize = 6
		encoded.WriteByte(minimumCodeSize)
		compressed := gifLZW(pixels, minimumCodeSize)
		for len(compressed) > 0 {
			size := min(255, len(compressed))
			encoded.WriteByte(byte(size))
			encoded.Write(compressed[:size])
			compressed = compressed[size:]
		}
		encoded.WriteByte(0)
	}
	encoded.WriteByte(0x3b)
	_, err := output.Write(encoded.Bytes())
	return err
}

func writeUint16(output *bytes.Buffer, value int) {
	var encoded [2]byte
	binary.LittleEndian.PutUint16(encoded[:], uint16(value))
	output.Write(encoded[:])
}

// gifLZW implements the GIF flavor of LZW: clear and end codes, 12-bit maximum
// dictionary entries, and least-significant-bit-first code packing.
func gifLZW(pixels []byte, minimumCodeSize int) []byte {
	clearCode := 1 << minimumCodeSize
	endCode := clearCode + 1
	dictionary := make(map[int]int, 4096)
	// hi is the most recently assigned dictionary code. It starts on the end
	// marker; the first emitted prefix advances it to the first free code. This
	// one-code lag is required by GIF decoders, which learn a new entry only when
	// they receive the following code.
	hi := endCode
	codeSize := minimumCodeSize + 1
	packer := bitPacker{}
	packer.write(clearCode, codeSize)
	if len(pixels) == 0 {
		packer.write(endCode, codeSize)
		return packer.finish()
	}

	prefix := int(pixels[0])
	for _, value := range pixels[1:] {
		key := prefix<<8 | int(value)
		if code, ok := dictionary[key]; ok {
			prefix = code
			continue
		}
		packer.write(prefix, codeSize)
		hi++
		if hi == 4095 {
			packer.write(clearCode, codeSize)
			dictionary = make(map[int]int, 4096)
			hi = endCode
			codeSize = minimumCodeSize + 1
		} else {
			if hi == 1<<codeSize && codeSize < 12 {
				codeSize++
			}
			dictionary[key] = hi
		}
		prefix = int(value)
	}
	packer.write(prefix, codeSize)
	hi++
	if hi == 1<<codeSize && codeSize < 12 {
		codeSize++
	}
	packer.write(endCode, codeSize)
	return packer.finish()
}

type bitPacker struct {
	bytes []byte
	bits  uint64
	count uint
}

func (packer *bitPacker) write(code, width int) {
	packer.bits |= uint64(code) << packer.count
	packer.count += uint(width)
	for packer.count >= 8 {
		packer.bytes = append(packer.bytes, byte(packer.bits))
		packer.bits >>= 8
		packer.count -= 8
	}
}

func (packer *bitPacker) finish() []byte {
	if packer.count > 0 {
		packer.bytes = append(packer.bytes, byte(packer.bits))
	}
	return packer.bytes
}

var visualPalette = color.Palette{
	hex("#0b1116"), hex("#0e151b"), hex("#121a20"), hex("#162129"),
	hex("#1c2932"), hex("#25343e"), hex("#2b3b46"), hex("#344652"),
	hex("#40535f"), hex("#526774"), hex("#657b87"), hex("#7a8e99"),
	hex("#8fa1aa"), hex("#a5b3ba"), hex("#bac5ca"), hex("#d1d9dc"),
	hex("#e6ecee"), hex("#f4f7f8"),
	hex("#123326"), hex("#19513a"), hex("#218055"), hex("#2fbf77"),
	hex("#45d98a"), hex("#8ce9b6"),
	hex("#102742"), hex("#17416f"), hex("#2364ab"), hex("#3987e5"),
	hex("#6da7ec"), hex("#a6c9f4"),
	hex("#3a2c0d"), hex("#765914"), hex("#bd8617"), hex("#fab219"),
	hex("#ffd46a"),
	hex("#401c25"), hex("#7f2a43"), hex("#bd3d65"), hex("#e55b86"),
	hex("#f39ab5"),
	hex("#402317"), hex("#803b22"), hex("#bd542b"), hex("#eb6834"),
	hex("#f6a27b"),
	hex("#1a3431"), hex("#22685d"), hex("#279b83"), hex("#42c8aa"),
	hex("#8ee5d2"),
	hex("#2b2043"), hex("#533b80"), hex("#7956ba"), hex("#a477e2"),
	hex("#c7a8ef"),
	hex("#441b18"), hex("#812e28"), hex("#bd4439"), hex("#ec6859"),
	hex("#f5a098"),
}

func hex(value string) color.Color {
	var r, g, b uint8
	_, _ = fmt.Sscanf(value, "#%02x%02x%02x", &r, &g, &b)
	return color.RGBA{R: r, G: g, B: b, A: 255}
}

func svgStart(title, status, accent string) *strings.Builder {
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`, width, height, width, height)
	out.WriteString(`<defs><filter id="glow" x="-80%" y="-80%" width="260%" height="260%"><feGaussianBlur stdDeviation="5" result="blur"/><feMerge><feMergeNode in="blur"/><feMergeNode in="SourceGraphic"/></feMerge></filter></defs>`)
	out.WriteString(`<rect width="634" height="557" fill="#0b1116"/>`)
	out.WriteString(`<path d="M18 86H616M18 165H616M18 244H616M18 323H616M18 402H616M18 481H616M112 42V535M206 42V535M300 42V535M394 42V535M488 42V535" stroke="#25343e" stroke-width="1" opacity=".24"/>`)
	out.WriteString(`<rect x="14" y="14" width="606" height="529" rx="12" fill="none" stroke="#2b3b46"/>`)
	text(&out, 30, 39, title, 11, 600, "#a5b3ba", "mono", "1.8")
	text(&out, 604, 39, status, 11, 600, accent, "mono", "1.2", `text-anchor="end"`)
	return &out
}

func svgEnd(out *strings.Builder) string {
	out.WriteString(`</svg>`)
	return out.String()
}

func text(out *strings.Builder, x, y int, value string, size, weight int, fill, family, tracking string, extra ...string) {
	font := "Noto Sans, sans-serif"
	if family == "mono" {
		font = "Noto Sans Mono, monospace"
	}
	attributes := ""
	if len(extra) > 0 {
		attributes = " " + extra[0]
	}
	fmt.Fprintf(out, `<text x="%d" y="%d" fill="%s" font-family="%s" font-size="%d" font-weight="%d" letter-spacing="%s"%s>%s</text>`, x, y, fill, font, size, weight, tracking, attributes, xml(value))
}

func box(out *strings.Builder, x, y, w, h, radius int, fill, stroke string, opacity float64) {
	fmt.Fprintf(out, `<rect x="%d" y="%d" width="%d" height="%d" rx="%d" fill="%s" stroke="%s" opacity="%.2f"/>`, x, y, w, h, radius, fill, stroke, opacity)
}

func line(out *strings.Builder, x1, y1, x2, y2 int, stroke string, opacity, width float64, dash string) {
	extra := ""
	if dash != "" {
		extra = ` stroke-dasharray="` + dash + `"`
	}
	fmt.Fprintf(out, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s" stroke-width="%.1f" opacity="%.2f"%s/>`, x1, y1, x2, y2, stroke, width, opacity, extra)
}

func dot(out *strings.Builder, x, y, radius int, fill string, opacity float64, glow bool) {
	filter := ""
	if glow {
		filter = ` filter="url(#glow)"`
	}
	fmt.Fprintf(out, `<circle cx="%d" cy="%d" r="%d" fill="%s" opacity="%.2f"%s/>`, x, y, radius, fill, opacity, filter)
}

func chip(out *strings.Builder, x, y, w int, label, colorValue string) {
	box(out, x, y, w, 30, 5, "#121a20", "#344652", 1)
	dot(out, x+14, y+15, 4, colorValue, 1, false)
	text(out, x+26, y+20, label, 11, 600, "#d1d9dc", "mono", ".2")
}

func progress(frame, total int) float64 {
	if total <= 1 {
		return 0
	}
	return float64(frame) / float64(total-1)
}

func pulse(frame, total int) float64 {
	return .55 + .45*math.Sin(progress(frame, total)*2*math.Pi-math.Pi/2)*.5 + .225
}

func movingX(start, end int, frame, total, offset int) int {
	position := math.Mod(progress(frame+offset, total)*1.25, 1)
	return start + int(float64(end-start)*position)
}

func xml(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(value)
}

func launchScene(frame, total int) string {
	out := svgStart("PKGREG / GO RELEASE", "NOW LIVE", "#45d98a")

	box(out, 28, 58, 578, 76, 8, "#121a20", "#2b3b46", 1)
	box(out, 42, 76, 38, 38, 7, "#0e151b", "#45d98a", 1)
	text(out, 52, 102, "[·]", 18, 700, "#45d98a", "mono", "-.8")
	text(out, 94, 91, "pkgreg", 24, 700, "#f4f7f8", "sans", "-.6")
	text(out, 94, 112, "ONE STATIC GO BINARY · NO CONTAINERS", 10, 600, "#8fa1aa", "mono", "1")
	box(out, 488, 79, 96, 32, 16, "#123326", "#2fbf77", 1)
	dot(out, 505, 95, 4, "#45d98a", pulse(frame, total), true)
	text(out, 518, 99, "READY", 10, 700, "#8ce9b6", "mono", "1.4")

	text(out, 317, 177, "DOWNLOAD ONCE.", 31, 750, "#f4f7f8", "sans", "-1.1", `text-anchor="middle"`)
	text(out, 317, 212, "BUILD MANY TIMES.", 31, 750, "#45d98a", "sans", "-1.1", `text-anchor="middle"`)
	text(out, 317, 237, "Six package roles. One verified client. One address.", 13, 400, "#a5b3ba", "sans", "0", `text-anchor="middle"`)

	labels := []struct {
		name, color string
	}{
		{"OCI", "#3987e5"}, {"PYPI", "#fab219"}, {"NPM", "#e55b86"},
		{"APT/APK", "#eb6834"}, {"GIT", "#a477e2"}, {"FILES", "#45d98a"},
	}
	for i, item := range labels {
		x := 33 + i*97
		chip(out, x, 271, 84, item.name, item.color)
		line(out, x+42, 301, 317, 341, "#344652", .65, 1, "4 6")
	}

	box(out, 222, 326, 190, 86, 9, "#162129", "#45d98a", 1)
	text(out, 317, 358, "pkgreg :8443", 18, 700, "#f4f7f8", "mono", "-.5", `text-anchor="middle"`)
	text(out, 317, 380, "TLS · PROXY · CONSOLE · API", 9, 600, "#8ce9b6", "mono", ".8", `text-anchor="middle"`)
	for i := 0; i < 8; i++ {
		fill := "#25343e"
		if i <= frame%9 {
			fill = "#45d98a"
		}
		box(out, 263+i*14, 391, 8, 8, 1, fill, fill, 1)
	}

	outputs := []string{"DEVELOPER SHELL", "CI BUILDERS", "OFFLINE SITE"}
	for i, label := range outputs {
		x := 34 + i*196
		line(out, 317, 412, x+86, 451, "#344652", .7, 1, "4 6")
		box(out, x, 449, 174, 54, 7, "#121a20", "#2b3b46", 1)
		dot(out, x+17, 466, 4, "#45d98a", 1, false)
		text(out, x+29, 470, label, 10, 700, "#d1d9dc", "mono", ".5")
		text(out, x+17, 490, []string{"TEMPORARY · VERIFIED", "LOCAL · REPEATABLE", "CACHE-ONLY · CLEAR"}[i], 8, 500, "#7a8e99", "mono", ".7")
	}

	x := movingX(75, 554, frame, total, 0)
	dot(out, x, 524, 3, "#45d98a", 1, true)
	line(out, 47, 524, 587, 524, "#2b3b46", .8, 1, "3 7")
	text(out, 47, 531, "HASH AS IT STREAMS", 8, 600, "#657b87", "mono", ".9")
	text(out, 587, 531, "COMMIT ONCE", 8, 600, "#657b87", "mono", ".9", `text-anchor="end"`)
	return svgEnd(out)
}

func cacheScene(frame, total int) string {
	out := svgStart("ONE CACHE / EVERY BUILD TOOL", "SINGLE-FLIGHT", "#6da7ec")
	text(out, 30, 72, "NORMAL COMMANDS", 9, 600, "#657b87", "mono", "1.3")
	text(out, 604, 72, "ONE :8443", 12, 600, "#d1d9dc", "mono", ".8", `text-anchor="end"`)

	rows := []struct {
		name, color string
	}{
		{"docker", "#3987e5"}, {"pip / uv", "#fab219"}, {"npm", "#e55b86"},
		{"apt / apk", "#eb6834"}, {"git", "#a477e2"}, {"files", "#45d98a"},
	}
	for i, item := range rows {
		y := 94 + i*65
		chip(out, 30, y, 108, item.name, item.color)
		line(out, 138, y+15, 258, y+15, "#344652", .8, 1, "7 8")
		line(out, 376, y+15, 492, y+15, "#344652", .8, 1, "7 8")
		if i == frame%len(rows) || i == (frame/3)%len(rows) {
			dot(out, movingX(148, 250, frame, total, i*2), y+15, 5, item.color, 1, true)
		}
		box(out, 492, y, 112, 30, 5, "#0e151b", "#2b3b46", 1)
		text(out, 506, y+20, []string{"OCI registry", "PyPI index", "npm registry", "HTTP mirror", "Git origin", "artifact path"}[i], 9, 500, "#8fa1aa", "mono", ".2")
	}

	box(out, 252, 84, 130, 416, 9, "#121a20", "#3987e5", 1)
	text(out, 317, 112, "pkgreg", 19, 700, "#f4f7f8", "sans", "-.3", `text-anchor="middle"`)
	text(out, 317, 131, "SHARED ENGINE", 9, 600, "#a6c9f4", "mono", "1", `text-anchor="middle"`)

	stages := []struct {
		name, note, color string
	}{
		{"HIT", "local entry", "#3987e5"},
		{"DEDUP", "same bytes", "#6da7ec"},
		{"PEER", "sibling cache", "#a6c9f4"},
		{"OFFLINE", "stop here", "#fab219"},
		{"MISS", "one origin", "#8fa1aa"},
	}
	active := frame / 4 % len(stages)
	for i, stage := range stages {
		y := 157 + i*55
		fill := "#162129"
		stroke := "#2b3b46"
		if i == active {
			fill = "#102742"
			stroke = stage.color
		}
		box(out, 270, y, 94, 42, 5, fill, stroke, 1)
		dot(out, 284, y+15, 4, stage.color, 1, i == active)
		text(out, 296, y+18, stage.name, 10, 700, "#e6ecee", "mono", ".7")
		text(out, 284, y+33, stage.note, 8, 500, "#7a8e99", "mono", ".2")
	}

	text(out, 317, 463, "ONE FETCH", 10, 700, "#45d98a", "mono", "1", `text-anchor="middle"`)
	text(out, 317, 480, "MANY READERS", 10, 700, "#45d98a", "mono", "1", `text-anchor="middle"`)

	box(out, 30, 507, 574, 24, 12, "#123326", "#218055", 1)
	text(out, 43, 523, "VERIFIED CLIENT SESSION", 9, 700, "#8ce9b6", "mono", ".8")
	text(out, 591, 523, "EXIT RESTORES YOUR SHELL", 9, 600, "#8ce9b6", "mono", ".6", `text-anchor="end"`)
	return svgEnd(out)
}

func checkpointScene(frame, total int) string {
	out := svgStart("CHECKPOINT LIVE / KEEP STORAGE BOUNDED", "PROJECT global", "#45d98a")

	box(out, 29, 64, 188, 430, 8, "#121a20", "#2b3b46", 1)
	text(out, 47, 91, "CONTENT-ADDRESSED STORE", 10, 700, "#d1d9dc", "mono", ".8")
	text(out, 47, 110, "ONE COPY OF EACH DIGEST", 8, 600, "#657b87", "mono", ".8")

	colors := []string{"#3987e5", "#fab219", "#e55b86", "#eb6834", "#a477e2", "#45d98a"}
	for row := 0; row < 12; row++ {
		for column := 0; column < 4; column++ {
			x := 49 + column*37
			y := 135 + row*26
			fill := "#162129"
			stroke := "#2b3b46"
			if (row*4+column+frame/2)%11 < 5 {
				fill = colors[(row+column)%len(colors)]
				stroke = fill
			}
			box(out, x, y, 23, 16, 2, fill, stroke, 1)
		}
	}
	text(out, 47, 474, "HASHED WHILE STREAMING", 8, 600, "#8fa1aa", "mono", ".6")

	text(out, 242, 83, "NATIVE CHECKPOINTS", 10, 700, "#d1d9dc", "mono", ".8")
	line(out, 251, 267, 436, 267, "#344652", 1, 2, "")
	versions := []string{"v12", "v13", "v14", "v15"}
	for i, version := range versions {
		x := 265 + i*54
		active := i <= frame/5
		fill := "#162129"
		stroke := "#40535f"
		if active {
			fill = "#19513a"
			stroke = "#45d98a"
		}
		dot(out, x, 267, 7, fill, 1, active && i == frame/5)
		fmt.Fprintf(out, `<circle cx="%d" cy="267" r="7" fill="none" stroke="%s"/>`, x, stroke)
		text(out, x, 289, version, 9, 600, "#8fa1aa", "mono", ".2", `text-anchor="middle"`)
	}
	box(out, 244, 116, 186, 104, 7, "#162129", "#45d98a", 1)
	text(out, 337, 145, "CHECKPOINT v15", 13, 700, "#f4f7f8", "mono", ".2", `text-anchor="middle"`)
	text(out, 337, 168, "SORTED MANIFEST", 9, 600, "#8ce9b6", "mono", ".8", `text-anchor="middle"`)
	text(out, 337, 189, "TRAFFIC KEEPS FLOWING", 9, 600, "#8fa1aa", "mono", ".5", `text-anchor="middle"`)
	box(out, 256, 325, 162, 72, 7, "#102742", "#3987e5", 1)
	text(out, 337, 350, "DELTA EXPORT", 11, 700, "#a6c9f4", "mono", ".8", `text-anchor="middle"`)
	text(out, 337, 374, "v14 → v15", 15, 700, "#f4f7f8", "mono", ".2", `text-anchor="middle"`)
	dot(out, movingX(268, 406, frame, total, 0), 387, 3, "#6da7ec", 1, true)
	line(out, 269, 387, 405, 387, "#2364ab", .8, 1, "4 5")
	text(out, 337, 431, "ROLL BACK ANY TIME", 9, 700, "#d1d9dc", "mono", ".9", `text-anchor="middle"`)
	text(out, 337, 450, "PINNED CONTENT SURVIVES", 8, 600, "#657b87", "mono", ".8", `text-anchor="middle"`)

	box(out, 454, 64, 151, 430, 8, "#121a20", "#2b3b46", 1)
	text(out, 472, 91, "BOUNDED STORAGE", 10, 700, "#d1d9dc", "mono", ".8")
	maintenance := []struct {
		name, value, color string
	}{
		{"QUOTA", "ATOMIC 507", "#fab219"},
		{"LRU", "HOT STAYS", "#3987e5"},
		{"TTL", "OLD GOES", "#a477e2"},
		{"GC", "ORPHANS", "#45d98a"},
		{"FLOOR", "FREE SPACE", "#eb6834"},
	}
	for i, item := range maintenance {
		y := 121 + i*62
		box(out, 472, y, 115, 45, 5, "#162129", "#344652", 1)
		dot(out, 487, y+15, 4, item.color, 1, i == frame/4%len(maintenance))
		text(out, 499, y+18, item.name, 10, 700, "#e6ecee", "mono", ".6")
		text(out, 487, y+35, item.value, 8, 600, "#7a8e99", "mono", ".6")
	}
	box(out, 472, 440, 115, 34, 5, "#123326", "#218055", 1)
	text(out, 530, 462, "ONLINE", 10, 700, "#8ce9b6", "mono", "1", `text-anchor="middle"`)
	return svgEnd(out)
}

func offlineScene(frame, total int) string {
	out := svgStart("PEER FIRST / AIR-GAP READY", "OFFLINE SERVE", "#fab219")

	text(out, 30, 70, "ONLINE SITE", 9, 700, "#657b87", "mono", "1.2")
	text(out, 604, 70, "DISCONNECTED SITE", 9, 700, "#fab219", "mono", "1.2", `text-anchor="end"`)

	box(out, 30, 91, 177, 365, 8, "#121a20", "#2b3b46", 1)
	text(out, 118, 122, "pkgreg A", 16, 700, "#f4f7f8", "mono", "-.3", `text-anchor="middle"`)
	text(out, 118, 143, "ORIGIN-FACING", 9, 600, "#8fa1aa", "mono", ".8", `text-anchor="middle"`)
	for i := 0; i < 24; i++ {
		x := 55 + (i%6)*22
		y := 176 + (i/6)*24
		fill := "#19513a"
		if i > 17+(frame/4)%6 {
			fill = "#25343e"
		}
		box(out, x, y, 13, 13, 2, fill, fill, 1)
	}
	box(out, 49, 303, 139, 55, 6, "#102742", "#3987e5", 1)
	text(out, 118, 326, "PEER ENDPOINT", 10, 700, "#a6c9f4", "mono", ".7", `text-anchor="middle"`)
	text(out, 118, 344, "DIGEST + RANGE", 8, 600, "#7a8e99", "mono", ".7", `text-anchor="middle"`)
	box(out, 49, 379, 139, 54, 6, "#162129", "#344652", 1)
	text(out, 118, 401, "PUBLIC ORIGIN", 10, 700, "#d1d9dc", "mono", ".7", `text-anchor="middle"`)
	text(out, 118, 420, "LAST RESORT", 8, 600, "#7a8e99", "mono", ".7", `text-anchor="middle"`)

	box(out, 244, 118, 146, 285, 8, "#121a20", "#3987e5", 1)
	text(out, 317, 149, "pkgreg B", 16, 700, "#f4f7f8", "mono", "-.3", `text-anchor="middle"`)
	text(out, 317, 170, "PEER-BEFORE-ORIGIN", 8, 600, "#a6c9f4", "mono", ".8", `text-anchor="middle"`)
	stages := []struct {
		name, color string
	}{
		{"HIT", "#3987e5"}, {"DEDUP", "#6da7ec"}, {"PEER", "#a6c9f4"},
		{"OFFLINE", "#fab219"}, {"MISS", "#8fa1aa"},
	}
	for i, item := range stages {
		y := 198 + i*37
		dot(out, 274, y, 4, item.color, 1, i == frame/5%len(stages))
		text(out, 289, y+4, item.name, 10, 700, "#d1d9dc", "mono", ".7")
	}
	box(out, 265, 371, 104, 18, 9, "#123326", "#218055", 1)
	text(out, 317, 383, "VERIFIED", 8, 700, "#8ce9b6", "mono", ".9", `text-anchor="middle"`)

	line(out, 207, 238, 244, 238, "#3987e5", .8, 2, "4 5")
	dot(out, movingX(211, 240, frame, total, 0), 238, 4, "#6da7ec", 1, true)
	text(out, 226, 228, "PEER", 7, 700, "#657b87", "mono", ".8", `text-anchor="middle"`)

	line(out, 421, 81, 421, 503, "#ec6859", .8, 1, "5 5")
	text(out, 429, 283, "AIR GAP", 9, 700, "#ec6859", "mono", "1.2", `transform="rotate(90 429 283)" text-anchor="middle"`)

	box(out, 450, 91, 155, 365, 8, "#121a20", "#bd8617", 1)
	text(out, 527, 122, "pkgreg OFFLINE", 14, 700, "#ffd46a", "mono", "-.3", `text-anchor="middle"`)
	text(out, 527, 143, "NO UPSTREAM DIALS", 9, 600, "#a5b3ba", "mono", ".7", `text-anchor="middle"`)
	for i := 0; i < 24; i++ {
		x := 469 + (i%6)*20
		y := 176 + (i/6)*23
		fill := "#765914"
		if i <= 15+(frame/6)%8 {
			fill = "#fab219"
		}
		box(out, x, y, 12, 12, 2, fill, fill, 1)
	}
	roles := []string{"docker", "pip / uv", "npm", "apt / apk", "git", "files"}
	for i, role := range roles {
		y := 297 + i*22
		dot(out, 470, y, 3, "#fab219", 1, false)
		text(out, 480, y+4, role, 9, 600, "#d1d9dc", "mono", ".3")
	}

	box(out, 391, 466, 123, 37, 6, "#3a2c0d", "#fab219", 1)
	text(out, 452, 490, "DELTA v15", 10, 700, "#ffd46a", "mono", ".9", `text-anchor="middle"`)
	dot(out, movingX(399, 506, frame, total, 0), 514, 4, "#fab219", 1, true)
	line(out, 399, 514, 590, 514, "#765914", 1, 1, "4 5")
	text(out, 590, 531, "IMPORT CHECKS DIGEST + LINEAGE", 8, 600, "#bd8617", "mono", ".6", `text-anchor="end"`)
	return svgEnd(out)
}
