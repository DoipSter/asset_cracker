// Package health serves a small JSON status page on localhost.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Report builds the status document. ok=false makes the endpoint answer 503.
type Report func(ctx context.Context) (doc any, ok bool)

// Serve answers GET /healthz until ctx ends.
func Serve(ctx context.Context, addr string, report Report) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		rctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		doc, ok := report(rctx)
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(doc)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
