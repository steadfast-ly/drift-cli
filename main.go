// Command drift is the command-line client for drift.
//
// Everything lives in cmd/; main exists only to own the process exit code,
// which is a public contract (see internal/cliexit).
package main

import (
	"os"

	"github.com/steadfast-ly/drift-cli/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
