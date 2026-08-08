package main

import (
	"flag"

	"github.com/brightskies/pkgreg/internal/config"
)

// bindConfigFlags wires the shared configuration flags onto a FlagSet.
//
// Each override is a pointer that stays nil unless the flag was actually given, so a
// flag never silently overwrites a config-file or environment value with a zero.
// flag.Visit after parsing is what distinguishes "absent" from "set to the default".
func bindConfigFlags(fs *flag.FlagSet) func() config.Flags {
	var (
		configFile  = fs.String("config", "", "path to a YAML config file")
		dataDir     = fs.String("data-dir", "", "state directory (default "+config.DefaultDataDir+")")
		unifiedAddr = fs.String("unified-addr", "", "TLS listener for docker/npm/pypi/git/files")
		proxyAddr   = fs.String("proxy-addr", "", "plain-HTTP listener for the apt/apk forward proxy")
		adminAddr   = fs.String("admin-addr", "", "listener for the console and control API")
		singlePort  = fs.Bool("single-port", false, "serve everything on one address by sniffing TLS")
		headless    = fs.Bool("headless", false, "do not serve the browser console; API and metrics stay up")
		logLevel    = fs.String("log-level", "", "debug|info|warn|error")
		logFormat   = fs.String("log-format", "", "json|text")
		offline     = fs.Bool("offline", false, "serve from cache only; never contact an upstream")
	)

	return func() config.Flags {
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

		out := config.Flags{ConfigFile: *configFile}
		if set["data-dir"] {
			out.DataDir = dataDir
		}
		if set["unified-addr"] {
			out.UnifiedAddr = unifiedAddr
		}
		if set["proxy-addr"] {
			out.ProxyAddr = proxyAddr
		}
		if set["admin-addr"] {
			out.AdminAddr = adminAddr
		}
		if set["single-port"] {
			out.SinglePort = singlePort
		}
		if set["headless"] {
			out.Headless = headless
		}
		if set["log-level"] {
			out.LogLevel = logLevel
		}
		if set["log-format"] {
			out.LogFormat = logFormat
		}
		if set["offline"] {
			out.Offline = offline
		}
		return out
	}
}
