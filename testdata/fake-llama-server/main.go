// Fake llama-server used by supervisor and daemon tests. Accepts (and ignores)
// the real llama-server flags vibe knows about, plus a handful of test-only
// flags that control /health behavior, premature exit, and signal handling.
package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	opts := parseLooseFlags(os.Args[1:])

	if opts.logStuff {
		fmt.Println("fake llama-server: starting on", opts.host, opts.port)
		fmt.Fprintln(os.Stderr, "fake llama-server: stderr log line")
	}

	if opts.ignoreSigint {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT)
		go func() {
			for range sig {
				// swallow
			}
		}()
	}

	started := time.Now()
	var ready atomic.Bool
	if opts.readyAfter == 0 && !opts.neverReady {
		ready.Store(true)
	} else if opts.readyAfter > 0 {
		go func() {
			time.Sleep(opts.readyAfter)
			ready.Store(true)
		}()
	}

	if opts.exitAfter > 0 {
		go func() {
			time.Sleep(opts.exitAfter)
			os.Exit(0)
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if opts.neverReady || !ready.Load() {
			http.Error(w, "loading", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, opts.alias)
	})

	_ = started // (reserved for future timing diagnostics)
	addr := fmt.Sprintf("%s:%d", opts.host, opts.port)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type options struct {
	host         string
	port         int
	alias        string
	readyAfter   time.Duration
	exitAfter    time.Duration
	neverReady   bool
	ignoreSigint bool
	logStuff     bool
}

// parseLooseFlags consumes the subset of llama-server flags vibe emits plus the
// test-only flags. Unknown flags are ignored (real llama-server has dozens we
// don't model, and extra_args may add more).
func parseLooseFlags(args []string) options {
	o := options{host: "127.0.0.1", alias: "fake"}
	takesValue := map[string]bool{
		"--model": true, "--alias": true, "--ctx-size": true, "--parallel": true,
		"--n-gpu-layers": true, "--cache-type-k": true, "--cache-type-v": true,
		"--host": true, "--port": true,
		"--ready-after": true, "--exit-after": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := ""
		if takesValue[a] && i+1 < len(args) {
			val = args[i+1]
		}
		switch a {
		case "--host":
			o.host = val
			i++
		case "--port":
			o.port, _ = strconv.Atoi(val)
			i++
		case "--alias":
			o.alias = val
			i++
		case "--ready-after":
			o.readyAfter, _ = time.ParseDuration(val)
			i++
		case "--exit-after":
			o.exitAfter, _ = time.ParseDuration(val)
			i++
		case "--never-ready":
			o.neverReady = true
		case "--ignore-sigint":
			o.ignoreSigint = true
		case "--log-stuff":
			o.logStuff = true
		default:
			if takesValue[a] {
				i++ // skip the value
			}
		}
	}
	return o
}
