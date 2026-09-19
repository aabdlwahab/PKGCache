package lockwarm

import (
	"slices"
	"testing"
)

func TestTargetSelectsOnlyInstallableWheels(t *testing.T) {
	linux := Target{OS: "linux", Arch: "x86_64", Python: 12}
	musl := Target{OS: "musllinux", Arch: "x86_64", Python: 12}
	mac := Target{OS: "macos", Arch: "arm64", Python: 12}
	windows := Target{OS: "windows", Arch: "x86_64", Python: 12}
	anyPython := Target{OS: "linux", Arch: "x86_64"}

	cases := []struct {
		filename string
		target   Target
		want     bool
	}{
		// Pure Python installs everywhere, whatever else the target says.
		{"idna-3.10-py3-none-any.whl", linux, true},
		{"idna-3.10-py3-none-any.whl", windows, true},

		// The ordinary case: one interpreter, one platform.
		{"numpy-2.1.0-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl", linux, true},
		{"numpy-2.1.0-cp311-cp311-manylinux_2_17_x86_64.manylinux2014_x86_64.whl", linux, false},
		{"numpy-2.1.0-cp312-cp312-manylinux_2_17_aarch64.whl", linux, false},
		{"numpy-2.1.0-cp312-cp312-macosx_11_0_arm64.whl", linux, false},

		// The compressed platform set matches if any of its members does.
		{"numpy-2.1.0-cp311-cp311-manylinux_2_17_x86_64.manylinux2014_x86_64.whl",
			anyPython, true},

		// abi3 is forward compatible and nothing else is.
		{"cryptography-43.0.0-cp39-abi3-manylinux_2_28_x86_64.whl", linux, true},
		{"cryptography-43.0.0-cp313-abi3-manylinux_2_28_x86_64.whl", linux, false},

		// musl and glibc are not interchangeable in either direction.
		{"regex-2024.9.11-cp312-cp312-musllinux_1_2_x86_64.whl", linux, false},
		{"regex-2024.9.11-cp312-cp312-musllinux_1_2_x86_64.whl", musl, true},
		{"regex-2024.9.11-cp312-cp312-manylinux_2_17_x86_64.whl", musl, false},

		// A fat macOS wheel carries the target's architecture inside it.
		{"pydantic_core-2.23-cp312-cp312-macosx_11_0_arm64.whl", mac, true},
		{"pydantic_core-2.23-cp312-cp312-macosx_10_12_universal2.whl", mac, true},
		{"pydantic_core-2.23-cp312-cp312-macosx_10_12_x86_64.whl", mac, false},
		{"pydantic_core-2.23-cp312-cp312-macosx_10_12_intel.whl",
			Target{OS: "macos", Arch: "x86_64", Python: 12}, true},

		{"xxhash-3.5.0-cp312-cp312-win_amd64.whl", windows, true},
		{"xxhash-3.5.0-cp312-cp312-win32.whl", windows, false},

		// Another interpreter entirely.
		{"greenlet-3.1-pp310-pypy310_pp73-manylinux_2_17_x86_64.whl", linux, false},

		// A build number sits between the version and the tags.
		{"pyzmq-26.2.0-1-cp312-cp312-manylinux_2_28_x86_64.whl", linux, true},

		// An unparseable name is warmed rather than guessed away.
		{"strange.whl", linux, true},
	}
	for _, c := range cases {
		if got := c.target.wantsWheel(c.filename); got != c.want {
			t.Errorf("%s for %s = %v, want %v", c.filename, c.target, got, c.want)
		}
	}
}

func TestSelectKeepsOneWheelAndFallsBackToSource(t *testing.T) {
	packages := []Package{{
		Name:     "numpy",
		Registry: "https://pypi.org/simple",
		Files: []File{
			{Filename: "numpy-2.1.0.tar.gz"},
			{Filename: "numpy-2.1.0-cp311-cp311-manylinux_2_17_x86_64.whl"},
			{Filename: "numpy-2.1.0-cp312-cp312-manylinux_2_17_x86_64.whl"},
			{Filename: "numpy-2.1.0-cp312-cp312-macosx_11_0_arm64.whl"},
			{Filename: "numpy-2.1.0-cp312-cp312-win_amd64.whl"},
		},
	}, {
		// Ships wheels, but none this target can use, and an sdist to build instead —
		// which is exactly what uv would do.
		Name:     "oldthing",
		Registry: "https://pypi.org/simple",
		Files: []File{
			{Filename: "oldthing-1.0.tar.gz"},
			{Filename: "oldthing-1.0-cp38-cp38-macosx_10_9_x86_64.whl"},
		},
	}, {
		// Nothing matches and there is nothing to build: the whole set is kept, because
		// warming too much is recoverable and warming nothing is not.
		Name:     "mysteryware",
		Registry: "https://pypi.org/simple",
		Files: []File{
			{Filename: "mysteryware-1.0-cp312-cp312-solaris_2_11_sparc.whl"},
		},
	}}
	target := Target{OS: "linux", Arch: "x86_64", Python: 12}

	got := Select(packages, target)
	want := [][]string{
		{"numpy-2.1.0-cp312-cp312-manylinux_2_17_x86_64.whl"},
		{"oldthing-1.0.tar.gz"},
		{"mysteryware-1.0-cp312-cp312-solaris_2_11_sparc.whl"},
	}
	for i, pkg := range got {
		names := make([]string, 0, len(pkg.Files))
		for _, file := range pkg.Files {
			names = append(names, file.Filename)
		}
		if !slices.Equal(names, want[i]) {
			t.Errorf("%s selected %v, want %v", pkg.Name, names, want[i])
		}
	}
	if Count(got) != 3 {
		t.Errorf("Count = %d, want 3", Count(got))
	}

	// Selecting is not allowed to disturb what the caller still needs whole: Rewrite is
	// handed the original set so that every URL in the file moves, warmed or not.
	if Count(packages) != 8 {
		t.Errorf("Select mutated its input: Count = %d, want 8", Count(packages))
	}
	if Count(Select(packages, Everything)) != 8 {
		t.Error("Everything dropped files")
	}
}

func TestParseTargetReadsWhatAPersonWouldType(t *testing.T) {
	cases := []struct {
		text string
		want Target
	}{
		{"all", Everything},
		{"linux/x86_64/cp312", Target{OS: "linux", Arch: "x86_64", Python: 12}},
		{"linux/amd64/3.12", Target{OS: "linux", Arch: "x86_64", Python: 12}},
		{"LINUX/X86_64/312", Target{OS: "linux", Arch: "x86_64", Python: 12}},
		{"macos/arm64", Target{OS: "macos", Arch: "arm64"}},
		{"macos/aarch64", Target{OS: "macos", Arch: "arm64"}},
		{"linux/aarch64/any", Target{OS: "linux", Arch: "aarch64"}},
		{"musllinux/x86_64", Target{OS: "musllinux", Arch: "x86_64"}},
		{"windows/amd64/cp313", Target{OS: "windows", Arch: "x86_64", Python: 13}},
	}
	for _, c := range cases {
		got, err := ParseTarget(c.text)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", c.text, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", c.text, got, c.want)
		}
	}
	for _, text := range []string{"linux", "plan9/x86_64", "linux/x86_64/py2", "a/b/c/d"} {
		if _, err := ParseTarget(text); err == nil {
			t.Errorf("ParseTarget(%q) succeeded", text)
		}
	}
	// auto describes this machine, whatever it is, and never selects everything.
	auto, err := ParseTarget("auto")
	if err != nil || auto.OS == "" || auto.Arch == "" {
		t.Errorf("ParseTarget(auto) = %+v, %v", auto, err)
	}
}

func TestTargetStringRoundTrips(t *testing.T) {
	for _, target := range []Target{
		Everything,
		{OS: "linux", Arch: "x86_64", Python: 12},
		{OS: "macos", Arch: "arm64"},
	} {
		got, err := ParseTarget(target.String())
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", target.String(), err)
		}
		if got != target {
			t.Errorf("%q parsed back as %+v", target.String(), got)
		}
	}
}
