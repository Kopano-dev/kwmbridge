/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package service

import (
	"context"
	"net/http"

	"github.com/gorilla/mux"
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
func (h *HTTPService) AddRoutes(ctx context.Context, router *mux.Router, wrapper func(http.Handler) http.Handler) http.Handler {
	v0 := router.PathPrefix(URIPrefix).Subrouter()
	if wrapper == nil {
		wrapper = func(next http.Handler) http.Handler {
			return next
		}
	}

	if mcucm, ok := h.services.MCUCManager.(*mcuc.Manager); ok {
		r := v0.PathPrefix("/bridge/mcuc").Subrouter()
		// /api/kwm/v0/bridge/mcuc/clients
		// /api/kwm/v0/bridge/mcuc/clients/:clientid/attached
		r.Handle("/clients", wrapper(mcucm.MakeHTTPMCUCHandler()))
	}

	// /api/kwm/v0/bridge/mcu/clients
	// /api/kwm/v0/bridge/mcu/clients/:clientid/attached
	// /api/kwm/v0/bridge/mcu/clients/:clientid/attached/:transaction
	// /api/kwm/v0/bridge/mcu/clients/:clientid/attached/:transaction/rtmcsfu
	// /api/kwm/v0/bridge/mcu/clients/:clientid/attached/:transaction/rtmcsfu/channel
	// /api/kwm/v0/bridge/mcu/clients/:clientid/attached/:transaction/rtmcsfu/channel/users
	// /api/kwm/v0/bridge/mcu/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/senders
	// /api/kwm/v0/bridge/mcu/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/connections
	// /api/kwm/v0/bridge/mcu/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/connections/:connectionid/tracks
	// /api/kwm/v0/bridge/mcu/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/connections/:connectionid/senders
	// /api/kwm/v0/bridge/mcu/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/connections/:connectionid/pending
	// /api/kwm/v0/bridge/mcu/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/connections/:connectionid/p2p/connections

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
