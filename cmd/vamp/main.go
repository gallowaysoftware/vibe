package main

import (
	"fmt"
	"os"

	"github.com/gallowaysoftware/vibe/internal/vamp/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "vamp:", err)
		os.Exit(1)
	}
}
