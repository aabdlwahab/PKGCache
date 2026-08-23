// Command pkgreg-bridge runs a verified pkgreg connection on loopback.
package main

import (
	"os"

	"github.com/aabdlwahab/PKGCache/internal/clientbridge"
)

func main() {
	os.Exit(clientbridge.Main(os.Args[1:], os.Stdout, os.Stderr))
}
