// Fake managed-frontend used by frontend/managed tests. Stands in for a
// native binary (Open-WebUI, etc.) supervised by the managed driver.
//
// Optional flags:
//
//	--listen <host>     bind host (default 127.0.0.1)
//	--port <int>        bind port (default 0 — picked by the OS; tests
//	                    supply a free port instead)
//	--ignore-sigint     swallow SIGINT so the managed driver has to
//	                    SIGKILL us; exercises the graceful-fallback path
//	--ready             serve "/" with a 200 immediately (default)
//	--never-ready       serve "/" with 503 indefinitely
//	--ready-after <d>   serve 503 for d, then 200
//	--echo-env <KEY>    print KEY=<value> from the environment on
//	                    startup; lets tests assert env propagation
//	--print-pwd         print the current working directory on startup;
//	                    lets tests assert workdir propagation
//
// Unknown flags are tolerated. The binary blocks until killed.
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

type options struct {
	host         string
	port         int
	ignoreSigint bool
	neverReady   bool
	readyAfter   time.Duration
	echoEnv      string
	printPwd     bool
}

func parseFlags(args []string) options {
	o := options{host: "127.0.0.1"}
	takesValue := map[string]bool{
		"--listen": true, "--port": true,
		"--ready-after": true, "--echo-env": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := ""
		if takesValue[a] && i+1 < len(args) {
			val = args[i+1]
		}
		switch a {
		case "--listen":
			o.host = val
			i++
		case "--port":
			o.port, _ = strconv.Atoi(val)
			i++
		case "--ignore-sigint":
			o.ignoreSigint = true
		case "--ready":
			// no-op; default behavior
		case "--never-ready":
			o.neverReady = true
		case "--ready-after":
			o.readyAfter, _ = time.ParseDuration(val)
			i++
		case "--echo-env":
			o.echoEnv = val
			i++
		case "--print-pwd":
			o.printPwd = true
		default:
			if takesValue[a] {
				i++
			}
		}
	}
	return o
}

func main() {
	opts := parseFlags(os.Args[1:])

	if opts.echoEnv != "" {
		fmt.Printf("%s=%s\n", opts.echoEnv, os.Getenv(opts.echoEnv))
	}
	if opts.printPwd {
		if pwd, err := os.Getwd(); err == nil {
			fmt.Printf("pwd=%s\n", pwd)
		}
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

	var ready atomic.Bool
	if !opts.neverReady {
		if opts.readyAfter > 0 {
			go func() {
				time.Sleep(opts.readyAfter)
				ready.Store(true)
			}()
		} else {
			ready.Store(true)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if opts.neverReady || !ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	if opts.port == 0 {
		// No port specified — block forever so tests that don't probe
		// can still rely on the process staying alive.
		select {}
	}
	addr := fmt.Sprintf("%s:%d", opts.host, opts.port)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
