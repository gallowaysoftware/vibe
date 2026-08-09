package daemon

import (
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// startRejection builds the daemon's refusal of a Start: a
// FailedPrecondition carrying msg for the human, and the typed
// StartRejection detail for the machine.
//
// The detail is the contract. vamp's candidate walk used to tell "this
// one does not fit, try a smaller one" apart from "your config is
// broken" by grepping the human message for "free VRAM"; 4a4c5ea
// rewrote that message and nothing noticed, because the only two tests
// covering the match each constructed the string they then asserted on.
// So the reason travels as data now, and vibeclient.IsVRAMRejection
// reads nothing else. internal/mutation pins the pair: drop the detail
// here and the daemon's own end-to-end classifier tests go red.
//
// A detail that cannot be marshalled would silently reproduce exactly
// the bug this replaces — a refusal that no caller can classify — so
// that path is loud rather than quiet. It is not reachable for a static
// generated message; it is logged because "impossible" is how the first
// one got here too.
func startRejection(msg string, detail *vibev1.StartRejection) *connect.Error {
	ce := connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
	d, err := connect.NewErrorDetail(detail)
	if err != nil {
		slog.Error("start rejection lost its typed reason; callers that fall back on it cannot classify this refusal",
			"reason", detail.GetReason().String(),
			"profile", detail.GetProfile(),
			"err", err)
		return ce
	}
	ce.AddDetail(d)
	return ce
}
