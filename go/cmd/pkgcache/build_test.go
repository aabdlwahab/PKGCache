package main

import (
	"flag"
	"reflect"
	"strings"
	"testing"
)

func TestSplitKnownFlagsKeepsOursAndHandsTheRestToDocker(t *testing.T) {
	newFlags := func() *flag.FlagSet {
		fs := flag.NewFlagSet("build", flag.ContinueOnError)
		fs.Bool("print", false, "")
		fs.String("project", "", "")
		return fs
	}
	cases := []struct {
		argv         []string
		ours, theirs []string
	}{
		// flag's own help flags are never registered, so they have to be recognised by
		// name or -h goes to docker after the cache has been started for it.
		{[]string{"-h"}, []string{"-h"}, nil},
		{[]string{"--help"}, []string{"--help"}, nil},
		{[]string{"-print", "-help"}, []string{"-print", "-help"}, nil},
		// A value flag owns the next argument unless it was written with =.
		{[]string{"-project", "web", "-t", "app", "."},
			[]string{"-project", "web"}, []string{"-t", "app", "."}},
		{[]string{"-project=web", "."}, []string{"-project=web"}, []string{"."}},
		{[]string{"-print", "-t", "app:dev", "."},
			[]string{"-print"}, []string{"-t", "app:dev", "."}},
		// Once an argument is docker's, everything after it is too — including a -h,
		// which is then docker's own help.
		{[]string{"-t", "app", "-h"}, nil, []string{"-t", "app", "-h"}},
		{[]string{"-", "-print"}, nil, []string{"-", "-print"}},
	}
	for _, tc := range cases {
		ours, theirs := splitKnownFlags(newFlags(), tc.argv)
		if !reflect.DeepEqual(ours, tc.ours) || !reflect.DeepEqual(theirs, tc.theirs) {
			t.Errorf("splitKnownFlags(%s) = %q, %q; want %q, %q",
				strings.Join(tc.argv, " "), ours, theirs, tc.ours, tc.theirs)
		}
	}
}
