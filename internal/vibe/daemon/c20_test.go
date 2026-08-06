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

// TestCellDrainWait_RidesOutAReconnectWithTheRequestStillRunning is the
// other half of the same rule, and the adversarial-review pass added it.
//
// C20 fixed the LABEL on a lost in-flight report and left the HARM: the
// wait gave up on the first missing tick, so a dropped events stream —
// llama-swap re-seeds a fresh connection with a current-state snapshot
// inside ~200 ms, and the watcher's backoff starts at 500 ms — became an
// immediate unit stop that force-closes whatever was generating. That is
// the exact outcome `--wait` exists to prevent, and fleetd had POSITIVE
// evidence one second earlier that a request was running.
//
// So a gap shorter than inflightEvidenceGrace is ridden out. Neither
// terminal answer moves: evidence that never returns is still
// skipped_no_inflight_data (the test above), and evidence that returns
// still has to reach zero before this reports `waited`.
func TestCellDrainWait_RidesOutAReconnectWithTheRequestStillRunning(t *testing.T) {
	cell := newInflightCell(t)
	cell.emit(1)
	d := drainWithFleet(t, cell)

	ran := make(chan struct{})
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) {
		close(ran)
		return "", nil
	})
	done := make(chan *connect.Response[vibev1.CellDrainResponse], 1)
	errc := make(chan error, 1)
	go func() {
		resp, err := d.CellDrain(context.Background(), connect.NewRequest(&vibev1.CellDrainRequest{WaitSeconds: 30}))
		errc <- err
		done <- resp
	}()

	// Parked on the in-flight request first: the drop has to land INSIDE
	// the wait loop, not before awaitQuiescence's pre-wait check.
	select {
	case <-ran:
		t.Fatal("drain command ran while a request was in flight")
	case <-time.After(300 * time.Millisecond):
	}

	// Drop the stream with the request still open and leave the count
	// missing for longer than one turn of the wait's 1 s ticker, so the
	// gap is genuinely OBSERVED rather than closed before anyone looked.
	// Then let the reconnect re-seed the same busy count, which is what a
	// real llama-swap does on its own.
	cell.srv.CloseClientConnections()
	select {
	case <-ran:
		t.Fatal("the drain ran during the evidence gap — a reconnect blip cancelled an in-flight generation, which is the outcome --wait exists to prevent")
	case <-time.After(1500 * time.Millisecond):
	}
	cell.emit(1)

	// Still parked, now on the RE-SEEDED busy count.
	select {
	case <-ran:
		t.Fatal("the drain ran after the in-flight report came back reporting a request still in flight")
	case <-time.After(1500 * time.Millisecond):
	}

	cell.emit(0)
	select {
	case resp := <-done:
		if err := <-errc; err != nil {
			t.Fatalf("drain: %v", err)
		}
		if resp.Msg.WaitStatus == nil || *resp.Msg.WaitStatus != fleetapi.DrainWaitWaited {
			t.Fatalf("wait_status = %v, want %q — the evidence came back and the cell then went quiet",
				resp.Msg.WaitStatus, fleetapi.DrainWaitWaited)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the drain never returned")
	}
	<-ran
}
