package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Serve serves the relay on ln until ctx ends, then lets open streams finish for up to Config.DrainTimeout
// and closes the rest. With a TLS certificate configured the relay serves TLS (HTTP/2 through ALPN); without
// one it serves cleartext HTTP/2 and HTTP/1.1 for a reverse proxy.
func (r *Relay) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No read or write timeout: the agent run is a stream that lasts as long as the conversation.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	if r.cfg.TLSCertFile == "" {
		srv.Protocols = new(http.Protocols)
		srv.Protocols.SetHTTP1(true)
		srv.Protocols.SetUnencryptedHTTP2(true)
	}
	errc := make(chan error, 1)
	go func() {
		if r.cfg.TLSCertFile != "" {
			errc <- srv.ServeTLS(ln, r.cfg.TLSCertFile, r.cfg.TLSKeyFile)
			return
		}
		errc <- srv.Serve(ln)
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	drain, cancel := context.WithTimeout(context.Background(), r.cfg.DrainTimeout)
	defer cancel()
	err := srv.Shutdown(drain)
	if errors.Is(err, context.DeadlineExceeded) {
		r.log.Warn("drain timeout: closing the streams still open")
		err = srv.Close()
	}
	if serveErr := <-errc; !errors.Is(serveErr, http.ErrServerClosed) && err == nil {
		err = serveErr
	}
	if err != nil {
		return fmt.Errorf("relay: shutdown: %w", err)
	}
	return nil
}
