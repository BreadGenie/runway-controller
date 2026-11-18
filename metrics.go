package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func startMetrics(address string, write func(io.Writer)) (func(), error) {
	if address == "" {
		return func() {}, nil
	}
	bind, err := netip.ParseAddrPort(address)
	if err != nil || !bind.Addr().IsLoopback() {
		return nil, errors.New("metrics_listen must be a loopback address and port")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for metrics: %w", err)
	}
	server := &http.Server{
		Handler: metricsHandler(write), ReadHeaderTimeout: time.Second,
		WriteTimeout: 3 * time.Second, IdleTimeout: time.Second,
	}
	go server.Serve(listener)
	return func() { _ = server.Close() }, nil
}

func metricsHandler(write func(io.Writer)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		write(w)
	})
	return mux
}

func metric(w io.Writer, name, kind, labels string, value any) {
	fmt.Fprintf(w, "# TYPE runway_%s %s\nrunway_%s%s %v\n", name, kind, name, labels, value)
}

func metricLabel(value string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"").Replace(value) + "\""
}

func cacheDiskMetrics(w io.Writer, journal, spool string) {
	fmt.Fprintln(w, "# TYPE runway_cache_filesystem_available_bytes gauge")
	fmt.Fprintln(w, "# TYPE runway_cache_filesystem_size_bytes gauge")
	for _, fs := range []struct{ name, path string }{{"journal", journal}, {"spool", spool}} {
		var stat unix.Statfs_t
		if unix.Statfs(fs.path, &stat) == nil {
			fmt.Fprintf(w, "runway_cache_filesystem_available_bytes{area=%q} %d\n", fs.name, uint64(stat.Bavail)*uint64(stat.Bsize))
			fmt.Fprintf(w, "runway_cache_filesystem_size_bytes{area=%q} %d\n", fs.name, uint64(stat.Blocks)*uint64(stat.Bsize))
		}
	}
}
