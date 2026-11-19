// The Go core's entrypoint: a plain HTTP server on a port it chooses or
// is given. In a trial the Node launcher (main.ts) spawns this binary,
// reads the port it prints, and owns the register/poll lifecycle; this
// binary only serves. For local development it serves on FORGE_PORT
// (default 8793) until killed, so the kit's suite runner can dial it
// directly.
//
//   FORGE_PORT=0     bind an ephemeral port and print it, then serve.
//   FORGE_PORT=8793  bind 8793 and serve.
//   FORGE_ADDR       bind address (default 127.0.0.1).
//
// GC posture: the vault's heap is small (tens of megabytes of objects
// and repos) against the entry's 1 GiB limit, and the suite is a burst
// of short-lived per-request garbage, so the collector is told to wait
// four times longer between cycles than the default. That trades a
// little headroom the process never uses for measurably less GC CPU in
// the hot loop.

package main

import (
	"fmt"
	"net"
	"os"
	"runtime/debug"
	"strconv"
)

func main() {
	debug.SetGCPercent(400)
	app := NewApp()
	port := 8793
	if p := os.Getenv("FORGE_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	addr := os.Getenv("FORGE_ADDR")
	if addr == "" {
		addr = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", addr+":"+strconv.Itoa(port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[forge] listening on port %d\n", actual)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go ServeConn(conn, app.Handle)
	}
}
