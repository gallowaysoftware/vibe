package main

import (
	"fmt"
	"os"

	"github.com/gallowaysoftware/vibe/internal/vibe/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		// A command may ask for a specific status when its own output is
		// the message (`vibe fleet doctor`: 1 FAIL / 2 WARN / 3 only
		// UNKNOWNs). Printing an error line on top of the report it just
		// rendered would bury the finding under a restatement of it.
		if code := cli.ExitCode(err); code != 0 {
			os.Exit(code)
		}
		fmt.Fprintln(os.Stderr, "vibe:", err)
		os.Exit(1)
	}
}
