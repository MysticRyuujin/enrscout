package debugsrv

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// Start serves pprof on addr (empty = off); bind loopback/private, never public.
// It binds synchronously so a bad production configuration fails at startup.
func Start(addr string) error {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen pprof %s: %w", addr, err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	go func() {
		slog.Info("pprof server listening", "addr", addr)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("pprof server stopped", "err", err)
		}
	}()
	return nil
}
