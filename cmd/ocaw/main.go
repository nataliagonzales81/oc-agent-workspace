// Command ocaw operates a project agent workspace. This file is deliberately
// thin: it detects whether stdout is a terminal, hands argv to the CLI, and
// exits with the code derived from the result envelope.
package main

import (
	"os"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr, cli.IsTerminal(os.Stdout)))
}
