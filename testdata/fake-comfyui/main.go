// Fake ComfyUI used by daemon tests. Stands in for `python3 main.py
// --listen <addr> --port <port>`: serves a minimal HTTP server that returns
// 200 on /system_stats so the supervisor's health probe transitions to
// ready. Unknown args are tolerated (real ComfyUI accepts dozens vibe
// doesn't model).
package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
)

func main() {
	host, port := "127.0.0.1", 0

	args := os.Args[1:]
	// The first positional arg (when launched from profile.ComfyUISpec) is
	// "main.py"; ignore it. Then look for --listen / --port.
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--listen":
			if i+1 < len(args) {
				host = args[i+1]
				i++
			}
		case "--port":
			if i+1 < len(args) {
				port, _ = strconv.Atoi(args[i+1])
				i++
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"system":{"os":"linux"},"devices":[]}`)
	})

	addr := fmt.Sprintf("%s:%d", host, port)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
