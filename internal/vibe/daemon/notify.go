package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetnotify"
)

// The C9 notifier's config wiring. Config errors are loud at startup
// (a typo'd alarm kind is an operator bug) and never stop the registry
// coming up — the same posture as the probe scheduler: fleetd is
// read-and-request-only, and a broken pager must not cost the fleet its
// control plane.

// startNotifyLoop builds the sink and starts the evaluator. A misparsed
// or unreadable endpoint disables notifications with a warning that
// never echoes the value back (it is a credential).
func (d *Daemon) startNotifyLoop(cfg Config) {
	nc := cfg.Fleet.Notify
	url, err := notifyURL(nc)
	if err != nil {
		slog.Warn("fleet notify disabled", "err", err)
		return
	}
	if url == "" {
		return
	}
	token, err := readSecretFile(nc.TokenFile)
	if err != nil {
		slog.Warn("fleet notify disabled: token file unreadable", "path", nc.TokenFile, "err", err)
		return
	}
	sink, err := fleetnotify.NewWebhookSink(fleetnotify.WebhookConfig{
		URL:    url,
		Token:  token,
		Format: nc.Format,
	})
	if err != nil {
		slog.Warn("fleet notify disabled", "err", err)
		return
	}
	policy, warnings := notifyPolicy(nc)
	for _, w := range warnings {
		slog.Warn("fleet notify config", "detail", w)
	}
	d.fleet.StartNotifyLoop(fleetapi.NotifyLoopConfig{
		Sink:     sink,
		Policy:   policy,
		Interval: parseDurationOr(nc.Interval, 0, "fleet.notify.interval"),
	})
}

// notifyURL resolves the endpoint from url_file (preferred) or url.
// Both set is an error rather than a silent precedence rule: which one
// won is exactly the question an operator cannot answer from a log line
// that must not print either value.
func notifyURL(nc NotifyConfig) (string, error) {
	inline := strings.TrimSpace(nc.URL)
	if nc.URLFile != "" && inline != "" {
		return "", errors.New("fleet.notify sets both url and url_file; pick one")
	}
	if nc.URLFile == "" {
		return inline, nil
	}
	v, err := readSecretFile(nc.URLFile)
	if err != nil {
		return "", fmt.Errorf("read fleet.notify.url_file %s: %w", nc.URLFile, err)
	}
	if v == "" {
		return "", fmt.Errorf("fleet.notify.url_file %s is empty", nc.URLFile)
	}
	return v, nil
}

// readSecretFile reads the first non-empty line of a file holding a
// credential. The VALUE is never logged by any caller; the path is.
func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			return v, nil
		}
	}
	return "", nil
}

// notifyPolicy validates the declared policy against fleetnotify's
// vocabulary, returning warnings rather than failing: an unknown alarm
// kind must not silently become "notify about everything" OR take the
// daemon down.
func notifyPolicy(nc NotifyConfig) (fleetnotify.Policy, []string) {
	var warnings []string
	p := fleetnotify.DefaultPolicy()
	if len(nc.Alarms) > 0 {
		var kinds []fleetnotify.Kind
		for _, name := range nc.Alarms {
			k, err := fleetnotify.ParseKind(strings.TrimSpace(name))
			if err != nil {
				warnings = append(warnings, err.Error()+"; ignored")
				continue
			}
			kinds = append(kinds, k)
		}
		if len(kinds) == 0 {
			warnings = append(warnings, "no valid alarm kinds configured; keeping the default set")
		} else {
			p.Alarms = kinds
		}
	}
	for name, v := range nc.Dwell {
		k, err := fleetnotify.ParseKind(strings.TrimSpace(name))
		if err != nil {
			warnings = append(warnings, err.Error()+"; dwell ignored")
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			warnings = append(warnings, fmt.Sprintf("dwell %q for %s is not a non-negative Go duration; ignored", v, name))
			continue
		}
		p.Dwell[k] = d
	}
	p.ClearDwell = parseDurationOr(nc.ClearDwell, p.ClearDwell, "fleet.notify.clear_dwell")
	if nc.RatePerHour > 0 {
		p.RatePerHour = nc.RatePerHour
	}
	if nc.Burst > 0 {
		p.Burst = nc.Burst
	}
	if nc.Resolve != nil {
		p.Resolve = *nc.Resolve
	}
	return p, warnings
}

func parseDurationOr(v string, fallback time.Duration, label string) time.Duration {
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("invalid duration; using the default", "field", label, "value", v)
		return fallback
	}
	return d
}
