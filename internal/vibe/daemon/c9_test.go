package daemon

// C9's config wiring: which endpoint wins, what an unreadable or absurd
// config does, and the one thing the daemon must never do with a webhook
// URL — print it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetnotify"
)

const notifySecret = "https://ntfy.example.invalid/vibe-fleet-SECRETTOPIC"

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestNotifyURL_PrefersTheFileAndRefusesToGuessBetweenBoth: which of two
// configured endpoints won is exactly the question an operator cannot
// answer from a log line that must not print either value, so setting
// both is an error rather than a precedence rule.
func TestNotifyURL_PrefersTheFileAndRefusesToGuessBetweenBoth(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "url", notifySecret+"\n")

	got, err := notifyURL(NotifyConfig{URLFile: path})
	if err != nil || got != notifySecret {
		t.Fatalf("url_file: got %q err %v", got, err)
	}
	got, err = notifyURL(NotifyConfig{URL: notifySecret})
	if err != nil || got != notifySecret {
		t.Fatalf("inline url: got %q err %v", got, err)
	}
	if _, err := notifyURL(NotifyConfig{URL: notifySecret, URLFile: path}); err == nil {
		t.Fatal("both url and url_file were accepted")
	}
	if _, err := notifyURL(NotifyConfig{URLFile: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("a missing url_file was accepted")
	}
	if _, err := notifyURL(NotifyConfig{URLFile: writeFile(t, dir, "empty", "\n\n")}); err == nil {
		t.Fatal("an empty url_file was accepted")
	}
	if got, err := notifyURL(NotifyConfig{}); err != nil || got != "" {
		t.Fatalf("unconfigured: got %q err %v", got, err)
	}
}

// TestNotifyPolicy_DefaultsToTheAlarmColumnAndWarnsOnGarbage: an unknown
// alarm kind must not silently become "notify about everything", and it
// must not take the daemon down either.
func TestNotifyPolicy_DefaultsToTheAlarmColumnAndWarnsOnGarbage(t *testing.T) {
	p, warns := notifyPolicy(NotifyConfig{})
	if len(warns) != 0 {
		t.Fatalf("an empty config warned: %v", warns)
	}
	if len(p.Alarms) != len(fleetnotify.DefaultAlarms()) {
		t.Fatalf("alarms = %v, want the default set", p.Alarms)
	}

	p, warns = notifyPolicy(NotifyConfig{Alarms: []string{"cell_absent", "everything"}})
	if len(warns) != 1 || !strings.Contains(warns[0], "everything") {
		t.Fatalf("warnings = %v", warns)
	}
	if len(p.Alarms) != 1 || p.Alarms[0] != fleetnotify.KindCellAbsent {
		t.Fatalf("alarms = %v, want only the valid one", p.Alarms)
	}

	// Every kind invalid: keep the default set rather than silently
	// disabling every alarm the operator asked for.
	p, warns = notifyPolicy(NotifyConfig{Alarms: []string{"nope"}})
	if len(p.Alarms) != len(fleetnotify.DefaultAlarms()) || len(warns) != 2 {
		t.Fatalf("alarms = %v warns = %v", p.Alarms, warns)
	}
}

func TestNotifyPolicy_AppliesDwellAndRateOverrides(t *testing.T) {
	p, warns := notifyPolicy(NotifyConfig{
		Dwell:       map[string]string{"cell_absent": "45s", "bogus": "1m", "fingerprint_drift": "not-a-duration"},
		ClearDwell:  "90s",
		RatePerHour: 3,
		Burst:       1,
		Resolve:     new(bool),
	})
	if p.Dwell[fleetnotify.KindCellAbsent] != 45*time.Second {
		t.Fatalf("cell_absent dwell = %v", p.Dwell[fleetnotify.KindCellAbsent])
	}
	if p.Dwell[fleetnotify.KindFingerprint] != 15*time.Minute {
		t.Fatalf("an unparseable dwell overwrote the default: %v", p.Dwell[fleetnotify.KindFingerprint])
	}
	if p.ClearDwell != 90*time.Second || p.RatePerHour != 3 || p.Burst != 1 || p.Resolve {
		t.Fatalf("policy = %+v", p)
	}
	if len(warns) != 2 {
		t.Fatalf("warnings = %v, want one per bad entry", warns)
	}
}

// TestNotifyConfigErrorsNeverEchoTheEndpoint: the config path is
// loggable, the value is not.
func TestNotifyConfigErrorsNeverEchoTheEndpoint(t *testing.T) {
	dir := t.TempDir()
	both, err := notifyURL(NotifyConfig{URL: notifySecret, URLFile: writeFile(t, dir, "url", notifySecret)})
	if err == nil {
		t.Fatalf("want an error, got %q", both)
	}
	if strings.Contains(err.Error(), "SECRETTOPIC") {
		t.Fatalf("the error echoed the credential: %v", err)
	}
	_, err = fleetnotify.NewWebhookSink(fleetnotify.WebhookConfig{URL: "ftp://x/" + "SECRETTOPIC"})
	if err == nil || strings.Contains(err.Error(), "SECRETTOPIC") {
		t.Fatalf("sink construction error = %v", err)
	}
}
