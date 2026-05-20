package vamp

import (
	"fmt"
	"os"

	internalvamp "github.com/gallowaysoftware/vibe/internal/vamp"
	"github.com/gallowaysoftware/vibe/internal/vamp/cli"
)

// Factory is the constructor your pipeline binary supplies to Main. It is
// called once per subcommand invocation so the pipeline reflects whatever
// the user passed on the CLI (e.g. flag-derived inputs your factory wants
// to read from os.Args). The returned *Pipeline should already be Built;
// callers that want validation deferred to here can return an unbuilt
// pipeline and rely on Main's Build pass.
type Factory func() (*Pipeline, error)

// Main is the canonical entry point a Go-DSL pipeline binary's `func main`
// calls. It mounts a cobra root with the four pipeline-bound subcommands
// (run / validate / viz / render) wired to the supplied factory, plus the
// pipeline-independent subcommands (list / logs / cache / jobs / runs /
// capabilities / schema / confirm / diff / cancel) the standalone `vamp`
// binary exposes. Errors are printed to stderr and the process exits with
// status 1; if you want non-exiting behaviour use MainE.
func Main(factory Factory) {
	if err := MainE(factory); err != nil {
		fmt.Fprintln(os.Stderr, "vamp:", err)
		os.Exit(1)
	}
}

// MainE is the non-exiting variant of Main. The cobra root is built and
// executed; any error (subcommand failure, factory error, etc.) is
// returned for the caller to handle.
func MainE(factory Factory) error {
	if factory == nil {
		return fmt.Errorf("vamp.Main: factory is nil")
	}
	name := "vamp-pipeline"
	if exe, err := os.Executable(); err == nil {
		name = baseName(exe)
	}
	root := cli.BuildRoot(name, func() (*internalvamp.Pipeline, error) {
		p, err := factory()
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, fmt.Errorf("pipeline factory returned nil")
		}
		// Validate again here even though Build typically already did:
		// LoadPipeline-returned pipelines are validated, hand-rolled ones
		// may have skipped Build. Idempotent + cheap.
		if err := p.inner.Validate(); err != nil {
			return nil, err
		}
		return p.inner, nil
	})
	return root.Execute()
}

// baseName is a tiny filepath.Base wrapper that strips a leading directory
// without dragging path/filepath into this file. The pipeline binary's
// help text just wants something like "rag-eval" instead of the full
// absolute path under /tmp/go-build/.../exe.
func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}
