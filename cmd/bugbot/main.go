// Command bugbot is the entrypoint for the Bugbot bug-finding harness.
package main

import (
	"fmt"
	"os"

	"github.com/dpoage/bugbot/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "bugbot:", err)
		os.Exit(1)
	}
}
