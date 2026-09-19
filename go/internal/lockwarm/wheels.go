package lockwarm

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// Target is one environment a locked package can be installed into: the platform a wheel
// has to be built for, and the interpreter it has to be tagged for.
//
// A uv.lock pins one version per package but lists every file the index holds for that
// version — every platform, architecture and Python ABI the publisher uploaded. On real
// locks that is thirteen files per package where an install uses one, so warming all of
// them moves two orders of magnitude more bytes than any single machine needs. Selecting
// against a target is how that is avoided without giving up knowing, before the lock is
// rewritten, that the cache can serve it.
//
// The zero Target selects everything, which is what -target all asks for.
type Target struct {
	// OS is "linux", "musllinux", "macos" or "windows". Empty selects every file.
	OS string
	// Arch is the wheel-tag spelling, not Go's: x86_64, aarch64, arm64, i686, armv7l,
	// ppc64le, s390x, riscv64.
	Arch string
	// Python is the CPython 3 minor version — 12 for 3.12. Zero matches any of them,
	// which is what to use when the interpreter is unknown: it still drops every wheel
	// for another platform, which is most of them.
	Python int
}

// Everything is the target that selects the whole lock, file for file.
var Everything = Target{}

// String renders the target the way ParseTarget reads it.
func (t Target) String() string {
	if t.OS == "" {
		return "all"
	}
	if t.Python == 0 {
		return t.OS + "/" + t.Arch + "/any"
	}
	return fmt.Sprintf("%s/%s/cp3%d", t.OS, t.Arch, t.Python)
}

// ParseTarget reads "<os>/<arch>[/<python>]", or the two words that stand for a whole
// target: "all" for every file, and "auto" (or "") for this machine.
//
// The python component is written the way the wheel tag is — cp312 — and 3.12 and 312
// are taken as the same thing, because both are what a person reaches for first.
func ParseTarget(text string) (Target, error) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "all":
		return Everything, nil
	case "", "auto":
		return LocalTarget(), nil
	}
	parts := strings.Split(strings.ToLower(strings.TrimSpace(text)), "/")
	if len(parts) < 2 || len(parts) > 3 {
		return Target{}, fmt.Errorf(
			"lockwarm: target %q is not <os>/<arch>[/<python>], all, or auto", text)
	}
	target := Target{OS: parts[0], Arch: normalizeArch(parts[0], parts[1])}
	switch target.OS {
	case "linux", "musllinux", "macos", "windows":
	default:
		return Target{}, fmt.Errorf(
			"lockwarm: unknown target OS %q (linux, musllinux, macos, windows)", parts[0])
	}
	if len(parts) == 3 && parts[2] != "any" {
		minor, err := parsePythonMinor(parts[2])
		if err != nil {
			return Target{}, err
		}
		target.Python = minor
	}
	return target, nil
}

// parsePythonMinor reads cp312, 3.12 or 312 as the CPython 3 minor version.
func parsePythonMinor(text string) (int, error) {
	digits := strings.TrimPrefix(strings.TrimPrefix(text, "cp"), "3.")
	digits = strings.TrimPrefix(digits, "3")
	minor, err := strconv.Atoi(digits)
	if err != nil || minor <= 0 {
		return 0, fmt.Errorf("lockwarm: %q is not a CPython 3 version (try cp312)", text)
	}
	return minor, nil
}

// LocalTarget describes the machine this is running on, with the interpreter left
// unknown. Callers that can find the Python uv will use should set Python themselves —
// see PythonMinor — because it roughly halves again what a platform match alone selects.
func LocalTarget() Target {
	target := Target{Arch: normalizeArch(goOSName(), runtime.GOARCH), OS: goOSName()}
	if target.OS == "linux" && onMusl() {
		target.OS = "musllinux"
	}
	return target
}

// goOSName maps GOOS onto the OS names used in a target.
func goOSName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	default:
		return "linux"
	}
}

// normalizeArch maps Go's architecture names onto the spellings that appear in wheel
// tags, and accepts those spellings unchanged so a -target the user typed works too.
//
// macOS is the one place the same silicon has two names: Python reports arm64 there and
// aarch64 everywhere else, and the wheel tags follow Python.
func normalizeArch(osName, arch string) string {
	switch arch {
	case "amd64", "x86_64":
		return "x86_64"
	case "arm64", "aarch64":
		if osName == "macos" {
			return "arm64"
		}
		return "aarch64"
	case "386", "i686", "x86":
		return "i686"
	case "arm", "armv7l":
		return "armv7l"
	default:
		return arch
	}
}

// onMusl reports whether this is a musl system. musl and glibc wheels are not
// interchangeable in either direction, so getting this wrong warms a set of wheels that
// cannot be installed.
func onMusl() bool {
	for _, loader := range []string{
		"/lib/ld-musl-x86_64.so.1", "/lib/ld-musl-aarch64.so.1",
		"/lib/ld-musl-armhf.so.1", "/lib/ld-musl-i386.so.1",
	} {
		if _, err := os.Stat(loader); err == nil {
			return true
		}
	}
	return false
}

// Select returns the packages carrying only the files this target could install: the
// wheels whose tags it satisfies, and the sdist only when no wheel matches, which is
// exactly when uv would build from source.
//
// A package where nothing matches at all keeps its whole file set. That is the safe
// direction to be wrong in: a platform this matcher has not been taught costs a few
// extra warmed files, where guessing such a package empty would leave the one wheel the
// install actually needs out of the cache, and the miss would surface later and
// somewhere else.
func Select(packages []Package, target Target) []Package {
	if target.OS == "" {
		return packages
	}
	out := make([]Package, 0, len(packages))
	for _, pkg := range packages {
		var wheels, sources []File
		for _, file := range pkg.Files {
			switch {
			case !strings.HasSuffix(file.Filename, ".whl"):
				sources = append(sources, file)
			case target.wantsWheel(file.Filename):
				wheels = append(wheels, file)
			}
		}
		switch {
		case len(wheels) > 0:
			pkg.Files = wheels
		case len(sources) > 0:
			pkg.Files = sources
		}
		out = append(out, pkg)
	}
	return out
}

// Count reports how many files a set of packages pins, which is the number of requests
// warming them takes.
func Count(packages []Package) int {
	files := 0
	for _, pkg := range packages {
		files += len(pkg.Files)
	}
	return files
}

// wantsWheel reports whether a wheel filename's PEP 425 tags name something this target
// can install.
func (t Target) wantsWheel(filename string) bool {
	pythons, abis, platforms, ok := wheelTags(filename)
	if !ok {
		// Not a name this understands. Warming it costs one file; skipping it could
		// cost the install.
		return true
	}
	return t.pythonOK(pythons, abis) && slices.ContainsFunc(platforms, t.platformOK)
}

// wheelTags splits a wheel filename into its three compressed tag sets.
//
// The name is {distribution}-{version}(-{build})?-{python}-{abi}-{platform}.whl, and the
// distribution has had its dashes normalized to underscores, so the last three
// dash-separated fields are the tags whether or not a build number is present. Each is a
// dot-separated set standing for every combination of its members.
func wheelTags(filename string) (pythons, abis, platforms []string, ok bool) {
	fields := strings.Split(strings.TrimSuffix(filename, ".whl"), "-")
	if !strings.HasSuffix(filename, ".whl") || len(fields) < 5 {
		return nil, nil, nil, false
	}
	tags := fields[len(fields)-3:]
	return strings.Split(tags[0], "."), strings.Split(tags[1], "."),
		strings.Split(tags[2], "."), true
}

// platformOK reports whether a platform tag names something this target runs.
//
// The version inside the tag — manylinux_2_17, macosx_11_0 — is deliberately not
// checked. It is the oldest system the wheel supports, and a machine too old for it
// would fail the install whatever this command warmed; treating it as a match costs at
// most one extra file on a package that ships several manylinux generations.
func (t Target) platformOK(tag string) bool {
	if tag == "any" {
		return true
	}
	switch t.OS {
	case "linux", "musllinux":
		if !strings.HasSuffix(tag, "_"+t.Arch) {
			return false
		}
		switch {
		case strings.HasPrefix(tag, "musllinux_"):
			return t.OS == "musllinux"
		case strings.HasPrefix(tag, "manylinux"):
			return t.OS == "linux"
		case strings.HasPrefix(tag, "linux_"):
			// An unqualified linux tag declares no libc. PyPI will not accept one, so
			// this is a wheel built locally or served by a private index, and the only
			// safe reading is that whoever put it in the lock meant this machine.
			return true
		}
		return false
	case "macos":
		return t.macOK(tag)
	case "windows":
		switch tag {
		case "win_amd64":
			return t.Arch == "x86_64"
		case "win32":
			return t.Arch == "i686"
		case "win_arm64":
			return t.Arch == "arm64" || t.Arch == "aarch64"
		}
		return false
	}
	return false
}

// macOK matches macosx_{major}_{minor}_{arch}, including the fat-binary arch names that
// carry more than one architecture in one wheel.
func (t Target) macOK(tag string) bool {
	if !strings.HasPrefix(tag, "macosx_") {
		return false
	}
	fields := strings.Split(tag, "_")
	if len(fields) < 4 {
		return false
	}
	switch arch := strings.Join(fields[3:], "_"); arch {
	case t.Arch, "universal2":
		return true
	case "universal", "intel", "fat64", "fat32", "fat":
		// Every one of these carries x86_64; none carries arm64.
		return t.Arch == "x86_64"
	}
	return false
}

// pythonOK reports whether a python tag set, read together with the ABI tag set, names
// an interpreter this target is.
func (t Target) pythonOK(pythons, abis []string) bool {
	if t.Python == 0 {
		return true
	}
	// A cp3X-abi3 wheel uses the stable ABI and runs on that CPython and every later
	// one, which is why the ABI set has to be read to answer this.
	stable := slices.Contains(abis, "abi3")
	for _, tag := range pythons {
		family, minor, ok := splitPythonTag(tag)
		if !ok {
			// pp (PyPy), jy, ip: a different interpreter, or CPython 2.
			continue
		}
		switch {
		case minor < 0:
			// Bare py3: pure Python, every 3.x.
			return true
		case family == "py":
			// py310 is pure Python needing at least 3.10.
			if minor <= t.Python {
				return true
			}
		case minor == t.Python:
			return true
		case stable && minor < t.Python:
			return true
		}
	}
	return false
}

// splitPythonTag reads cp312 as ("cp", 12) and py3 as ("py", -1). Anything that is not
// CPython-or-generic 3 is reported as no match at all.
func splitPythonTag(tag string) (family string, minor int, ok bool) {
	for _, prefix := range []string{"cp3", "py3"} {
		rest, found := strings.CutPrefix(tag, prefix)
		if !found {
			continue
		}
		if rest == "" {
			return prefix[:2], -1, true
		}
		value, err := strconv.Atoi(rest)
		if err != nil {
			return "", 0, false
		}
		return prefix[:2], value, true
	}
	return "", 0, false
}
