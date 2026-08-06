package daemon

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// fleet-control C20, class 1: absent evidence read as a healthy value.
//
// awaitQuiescence checked "has this cell ever reported an in-flight
// count" ONCE, before the wait, and then spelled every subsequent read
// `n, _ := d.fleet.InFlight(cell)`. The count can go back to UNREPORTED
// mid-wait — the cell's events stream drops (clearInFlight) or sends a
// frame this build cannot fold (disarmInFlightLocked) — and the dropped
// bool turned that into n == 0, which this loop reads as quiescence and
// reports to the operator as `waited`.
//
// The stream drop is not exotic: it is what a cell restart, a network
// blip or an llama-swap upgrade looks like, and `drain --wait` exists
// precisely because the stop that follows cancels nothing gracefully
// past llama-swap's 30 s force-close. Claiming quiescence from the loss
// of the evidence is the one answer this path may never give.
//
// The migration to observed.Value[int] is what made it visible: there is
// no longer a way to read the number without answering the question.
func TestCellDrainWait_DoesNotClaimQuiescenceWhenTheEvidenceStops(t *testing.T) {
	cell := newInflightCell(t)
	cell.emit(1) // busy when the watcher connects
	d := drainWithFleet(t, cell)

	ran := make(chan struct{})
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) {
		close(ran)
		return "", nil
	})
	type result struct {
		resp *connect.Response[vibev1.CellDrainResponse]
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := d.CellDrain(context.Background(), connect.NewRequest(&vibev1.CellDrainRequest{WaitSeconds: 30}))
		done <- result{resp, err}
	}()

	// Parked on the in-flight request, exactly as TestCellDrain_WaitForQuiescence
	// establishes. Without this the rest proves nothing: a drain that never
	// waited would also "not claim quiescence".
	select {
	case <-ran:
		t.Fatal("drain command ran while a request was in flight")
	case <-time.After(300 * time.Millisecond):
	}

	// The evidence stops without the request finishing. clearInFlight
	// drops the count on the stream's end; the reconnect brings no frame,
	// because the cell sends one only when this test asks it to.
	cell.srv.CloseClientConnections()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("drain: %v", got.err)
		}
		if got.resp.Msg.WaitStatus == nil {
			t.Fatal("no wait_status: an operator who asked for quiescence cannot tell what happened")
		}
		if *got.resp.Msg.WaitStatus == fleetapi.DrainWaitWaited {
			t.Fatal("the drain reported `waited` after the in-flight report stopped — it waited for the evidence to disappear, not for the cell to go quiet")
		}
		if *got.resp.Msg.WaitStatus != fleetapi.DrainWaitSkippedNoInflight {
			t.Fatalf("wait_status = %q, want %q", *got.resp.Msg.WaitStatus, fleetapi.DrainWaitSkippedNoInflight)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the drain never returned")
	}
	<-ran
}
