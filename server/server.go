/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/longsleep/go-metrics/loggedwriter"
	"github.com/longsleep/go-metrics/timing"
	"github.com/sirupsen/logrus"
	kcoidc "stash.kopano.io/kc/libkcoidc"

	cfg "stash.kopano.io/kwm/kwmbridge/config"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm/mcu"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm/mcu/plugins/rtmcsfu"
)

// Server is our HTTP server implementation.
type Server struct {
	config *cfg.Config

	listenAddr string
	logger     logrus.FieldLogger

	requestLog bool
}

// NewServer constructs a server from the provided parameters.
func NewServer(c *cfg.Config) (*Server, error) {
	s := &Server{
		config: c,

		listenAddr: c.ListenAddr,
		logger:     c.Logger,
	}

	return s, nil
}

// WithMetrics adds metrics logging to the provided http.Handler. When the
// handler is done, the context is canceled, logging metrics.
func (s *Server) WithMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		// Create per request cancel context.
		ctx, cancel := context.WithCancel(req.Context())

		if s.requestLog {
			loggedWriter := metrics.NewLoggedResponseWriter(rw)
			// Create per request context.
			ctx = timing.NewContext(ctx, func(duration time.Duration) {
				// This is the stop callback, called when complete with duration.
				durationMs := float64(duration) / float64(time.Millisecond)
				// Log request.
				s.logger.WithFields(logrus.Fields{
					"status":     loggedWriter.Status(),
					"method":     req.Method,
					"path":       req.URL.Path,
					"remote":     req.RemoteAddr,
					"duration":   durationMs,
					"referer":    req.Referer(),
					"user-agent": req.UserAgent(),
					"origin":     req.Header.Get("Origin"),
				}).Debug("HTTP request complete")
			})
			rw = loggedWriter
		}

		// Run the request.
		next.ServeHTTP(rw, req.WithContext(ctx))

		// Cancel per request context when done.
		cancel()
	})
}

// AddContext adds the accociated server's context to the provided http.Hander
// request.
func (s *Server) AddContext(parent context.Context, next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		next.ServeHTTP(rw, req.WithContext(parent))
	})
}

// AddRoutes add the accociated Servers URL routes to the provided router with
// the provided context.Context.
func (s *Server) AddRoutes(ctx context.Context, router *mux.Router) http.Handler {
	// TODO(longsleep): Add subpath support to all handlers and paths.
	router.Handle("/health-check", s.WithMetrics(http.HandlerFunc(s.HealthCheckHandler)))

	return router
}

// Serve starts all the accociated servers resources and listeners and blocks
// forever until signals or error occurs. Returns error and gracefully stops
// all HTTP listeners before return.
func (s *Server) Serve(ctx context.Context) error {
	var err error

	serveCtx, serveCtxCancel := context.WithCancel(ctx)
	defer serveCtxCancel()

	logger := s.logger

	// OpenID connect.
	var oidcp *kcoidc.Provider
	if s.config.Iss != nil {
		var kcoidcLogger *debugLogger
		kcoidcDebug := os.Getenv("KCOIDC_DEBUG") == "1"
		if kcoidcDebug && logger != nil {
			kcoidcLogger = &debugLogger{
				logger: logger,
				prefix: "kcoidc debug ",
			}
		}

		if kcoidcLogger != nil {
			oidcp, err = kcoidc.NewProvider(s.config.HTTPClient, kcoidcLogger, kcoidcDebug)
		} else {
			oidcp, err = kcoidc.NewProvider(s.config.HTTPClient, nil, kcoidcDebug)
		}
		if err != nil {
			return fmt.Errorf("failed to create kcoidc provider for server: %v", err)
		}
		err = oidcp.Initialize(serveCtx, s.config.Iss)
		if err != nil {
			return fmt.Errorf("OIDC provider initialization error: %v", err)
		}
		if errOIDCInitialize := oidcp.WaitUntilReady(serveCtx, 10*time.Second); errOIDCInitialize != nil {
			// NOTE(longsleep): Do not treat this as error - just log.
			logger.WithError(errOIDCInitialize).WithField("iss", s.config.Iss).Warnf("failed to initialize OIDC provider")
		} else {
			logger.WithField("iss", s.config.Iss).Debugln("OIDC provider initialized")
		}
	}

	// HTTP services.
	router := mux.NewRouter()

	// Basic routes provided by server.
	s.AddRoutes(ctx, router)

	errCh := make(chan error, 2)
	exitCh := make(chan bool, 1)
	signalCh := make(chan os.Signal, 1)

	// HTTP listener.
	logger.WithField("listenAddr", s.listenAddr).Infoln("starting http listener")
	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return err
	}

	wg := &sync.WaitGroup{}

	srv := &http.Server{
		Handler: s.AddContext(serveCtx, router),
	}
	wg.Add(1)
	go func() {
		defer func() {
			logger.Debugln("http listener stopped")
			wg.Done()
		}()

		serveErr := srv.Serve(listener)
		if serveErr != nil {
			errCh <- serveErr
		}
	}()

	wg.Add(1)
	go func() {
		defer func() {
			logger.Debugln("kwm mcu connector stopped")
			wg.Done()
		}()

		// TODO(longsleep): Add support for multiple.
		uri := s.config.KWMServerURIs[0]
		logger.WithField("url", uri).Infoln("creating kwm client")
		kwmc, err := kwm.NewClient(uri, &kwm.Config{
			Config: s.config,

			HTTPClient: s.config.HTTPClient,
			Logger:     s.config.Logger.WithField("url", uri),
		})
		if err != nil {
			errCh <- err
			return
		}

		mcuc, err := mcu.NewClient(kwmc, &mcu.Options{
			Config: s.config,

			HTTPClient: s.config.HTTPClient,
			Logger:     s.config.Logger.WithField("url", uri),

			AttachPluginFactoryFunc: rtmcsfu.New,
		})
		if err != nil {
			errCh <- err
		}

		for {
			logger.WithField("url", uri).Infoln("connecting to kwm mcu API")
			err = mcuc.Start(serveCtx) // Connect and reconnect, this blocks.
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.WithError(err).Warnln("kwm mcu API connection stopped with error, restart scheduled")
			}
			select {
			case <-serveCtx.Done():
				return
			case <-time.After(1 * time.Second):
				logger.Infoln("reconnecting to kwm mcu API")
				// breaks and continues.
			}
		}
	}()

	go func() {
		wg.Wait()
		close(exitCh)
	}()

	logger.Infoln("ready to handle requests")

	// Wait for exit or error.
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err = <-errCh:
		// breaks
	case reason := <-signalCh:
		logger.WithField("signal", reason).Warnln("received signal")
		// breaks
	}

	// Shutdown, server will stop to accept new connections, requires Go 1.8+.
	logger.Infoln("clean server shutdown start")
	shutDownCtx, shutDownCtxCancel := context.WithTimeout(ctx, 10*time.Second)
	if shutdownErr := srv.Shutdown(shutDownCtx); shutdownErr != nil {
		logger.WithError(shutdownErr).Warn("clean server shutdown failed")
	}

	// Cancel our own context, wait on managers.
	serveCtxCancel()
	func() {
		for {
			select {
			case <-exitCh:
				return
			default:
				logger.Info("waiting for services to exit")
			}

			select {
			case reason := <-signalCh:
				logger.WithField("signal", reason).Warn("received signal")
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	shutDownCtxCancel() // prevent leak.

	return err
}
