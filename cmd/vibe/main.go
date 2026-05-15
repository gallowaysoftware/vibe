// vibe is a task-oriented launcher for local AI inference.
//
// This entry point is intentionally minimal during Phase 1 development; the
// CLI surface is wired up in internal/cli once enough of the daemon and
// supervisor are in place to do useful work.
package main

import "fmt"

func main() {
	fmt.Println("vibe (Phase 1, in development — see github.com/gallowaysoftware/vibe)")
}
