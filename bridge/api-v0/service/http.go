/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package service

import (
	"context"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/justinas/alice"
	"github.com/sirupsen/logrus"

	"stash.kopano.io/kwm/kwmbridge/bridge"
	"stash.kopano.io/kwm/kwmbridge/bridge/mcuc"
)

const (
	URIPrefix = "/api/kwm/v0"
)

// HTTPService binds the HTTP router with handlers for kwm API v1.
type HTTPService struct {
	logger   logrus.FieldLogger
	services *bridge.Services
}

// NewHTTPService creates a new HTTPService  with the provided options.
func NewHTTPService(ctx context.Context, logger logrus.FieldLogger, services *bridge.Services) *HTTPService {
	return &HTTPService{
		logger:   logger,
		services: services,
	}
}

// AddRoutes configures the services HTTP end point routing on the provided
// context and router.
func (h *HTTPService) AddRoutes(ctx context.Context, router *mux.Router, chain alice.Chain) http.Handler {
	v0 := router.PathPrefix(URIPrefix).Subrouter()

	if mcucm, ok := h.services.MCUCManager.(*mcuc.Manager); ok {
		r := v0.PathPrefix("/bridge/mcuc").Subrouter()

		// /api/kwm/v0/bridge/mcuc/clients
		// /api/kwm/v0/bridge/mcuc/clients/:client/attached
		// /api/kwm/v0/bridge/mcuc/clients/:client/attached/:transaction
		// /api/kwm/v0/bridge/mcuc/clients/:client/attached/:transaction/:bridge
		r.Handle("/clients", chain.ThenFunc(mcucm.HTTPClientsHandler))
		r.Handle("/clients/{clientID}", chain.ThenFunc(mcucm.HTTPClientsHandler))
		r.Handle("/clients/{clientID}/attached", chain.ThenFunc(mcucm.HTTPClientsAttachedHandler))
		r.Handle("/clients/{clientID}/attached/{attachedID}", chain.ThenFunc(mcucm.HTTPClientsAttachedHandler))
		r.PathPrefix("/clients/{clientID}/attached/{attachedID}/{bridgeID}").Handler(chain.ThenFunc(mcucm.HTTPClientsAttachedBridgeHandler))
	}

	return router
}

// NumActive returns the number of the currently active connections at the
// accociated HTTPService.
func (h *HTTPService) NumActive() (active uint64) {
	for _, service := range h.services.Services() {
		active += service.NumActive()
	}

	return active
}
