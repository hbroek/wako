// Command wako is a command-line client for a Wako server.
package main

import (
	"os"

	"github.com/hbroek/wako/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}
