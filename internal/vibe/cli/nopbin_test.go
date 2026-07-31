package cli

import "os/exec"

// trueBin / falseBin are the no-op success and failure programs the
// installer stubs exec instead of really running git/pip/hf. They are
// resolved through PATH rather than hardcoded to /bin: macOS ships both in
// /usr/bin, so the literal "/bin/true" made every installer test fail on an
// Apple-silicon dev box with "fork/exec /bin/true: no such file".
var (
	trueBin  = lookPathOr("true", "/bin/true")
	falseBin = lookPathOr("false", "/bin/false")
)

func lookPathOr(name, fallback string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return fallback
}
