package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	log "github.com/sirupsen/logrus"
)

func main() {
	var listenAddr string
	flag.StringVar(&listenAddr, "listen", ":18080", "Listen address")
	flag.Parse()

	mock := helps.NewCodexUpstreamMock()
	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mock.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.WithField("listen", listenAddr).Info("starting codex mock upstream server")
	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-signals:
		log.WithField("signal", sig.String()).Info("shutting down codex mock upstream server")
	case err := <-errCh:
		log.WithError(err).Error("codex mock upstream server failed")
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.WithError(err).Error("codex mock upstream server shutdown failed")
	}
}
