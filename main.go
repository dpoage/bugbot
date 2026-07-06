// Command bugbot is the entrypoint for the Bugbot bug-finding harness.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/dpoage/bugbot/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "bugbot:", err)
		var gateErr *cli.GateError
		if errors.As(err, &gateErr) {
			os.Exit(cli.ExitGateFailure)
		}
		os.Exit(cli.ExitError)
	}
}
