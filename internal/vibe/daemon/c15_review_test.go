package daemon

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// fleet-control C15, adversarial-review pass: keying a NON-front cell is
// a configuration C15 accepts and does not cover. The cell's own
// announcer, prober and usage collector dial its llama-swap without a
// credential, and `announceOnce` maps a gatherModels failure to an EMPTY
// model list — the exact input C4's warm policy reads as "nothing
// resident". Said once, at config load, to the person who just wrote it.
func TestCheckSwapKeys_NamesTheCellSideGap(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "swap.key")
	if err := os.WriteFile(key, []byte("k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		fleetcfg.FrontCell: {URL: "http://front", Class: fleetcfg.ClassAlwaysOn, SwapKeyFile: key},
		"heavy":            {URL: "http://heavy", Class: fleetcfg.ClassAlwaysOn, SwapKeyFile: key},
	}}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	checkSwapKeys(hosts)
	got := buf.String()
	if !strings.Contains(got, "CELL-side dialers") || !strings.Contains(got, "heavy") {
		t.Errorf("a keyed peer cell produced no warning about the uncovered cell-side dialers:\n%s", got)
	}
	if strings.Contains(got, "cells=[front heavy]\" ") {
		t.Error("the front was named in the cell-side warning; it has no announcer to break")
	}
	if strings.Contains(got, "k\n") {
		t.Error("the key value reached a log line")
	}
}

// A fleet that keys only the front — the reference posture this phase
// ships — must not produce the warning at all.
func TestCheckSwapKeys_FrontOnlyIsQuiet(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "swap.key")
	if err := os.WriteFile(key, []byte("k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		fleetcfg.FrontCell: {URL: "http://front", Class: fleetcfg.ClassAlwaysOn, SwapKeyFile: key},
		"heavy":            {URL: "http://heavy", Class: fleetcfg.ClassAlwaysOn},
	}}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	checkSwapKeys(hosts)
	if strings.Contains(buf.String(), "CELL-side dialers") {
		t.Errorf("the reference posture (front keyed, cells not) warned about nothing:\n%s", buf.String())
	}
}
