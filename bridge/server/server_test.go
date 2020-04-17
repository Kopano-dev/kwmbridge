/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gorilla/mux"
	"github.com/justinas/alice"
	"github.com/sirupsen/logrus"

	cfg "stash.kopano.io/kwm/kwmbridge/config"
)

var logger = &logrus.Logger{
	Out:       os.Stderr,
	Formatter: &logrus.TextFormatter{DisableColors: true},
	Level:     logrus.DebugLevel,
}

func newTestServer(ctx context.Context, t *testing.T) (*httptest.Server, *Server, http.Handler, *cfg.Config) {
	config := &cfg.Config{
		Logger: logger,
	}

	server, err := NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	router := mux.NewRouter()
	server.AddRoutes(ctx, router, alice.New())

	s := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		router.ServeHTTP(rw, req)
	}))

	return s, server, router, config
}

func TestNewTestServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTestServer(ctx, t)
}
