package main

import (
	"fmt"
	"os"

	"github.com/gallowaysoftware/vibe/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "vibe:", err)
		os.Exit(1)
	}
}
