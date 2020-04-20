/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package mcuc

import (
	"net/http"
	"strings"

	api "stash.kopano.io/kwm/kwmbridge/bridge/api-v0"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm/mcu"
)

func (m *Manager) HTTPClientsHandler(rw http.ResponseWriter, req *http.Request) {
	clientID, _ := api.GetRequestVar(req, "clientID")

	var resource interface{}
	if clientID == "" {
		var clients []interface{}

		for _, client := range m.clients {
			clients = append(clients, client.Resource())
		}

		resource = api.NewCollectionResource(clients, req, nil)
	} else {
		resource = m.getClientResourceOrWriteError(clientID, rw)
		if resource == nil {
			return
		}
		resource = api.NewItemResource(resource, req)
	}

	if writeErr := api.WriteResourceAsJSON(rw, resource); writeErr != nil {
		m.logger.WithError(writeErr).Errorln("failed to write json response")
	}
}

func (m *Manager) getClientResourceOrWriteError(clientID string, rw http.ResponseWriter) *mcu.ClientResource {
	client := func() *mcu.ClientResource {
		for _, c := range m.clients {
			if c.ID() == clientID {
				return c.Resource()
			}
		}
		return nil
	}()
	if client == nil {
		if writeErr := api.WriteErrorAsJSON(rw, api.NewErrorWithCodeAndMessage(
			"ErrorMessageClientNotfound",
			"The specified client was not found",
			api.ErrNotFound,
		)); writeErr != nil {
			m.logger.WithError(writeErr).Errorln("failed to write json error")
		}
		return nil
	}
	return client
}

func (m *Manager) HTTPClientsAttachedHandler(rw http.ResponseWriter, req *http.Request) {
	clientID, _ := api.GetRequestVar(req, "clientID")

	client := m.getClientResourceOrWriteError(clientID, rw)
	if client == nil {
		return
	}

	attachedID, _ := api.GetRequestVar(req, "attachedID")
	attached := m.getAttachedResourceOrWriteError(client, attachedID, rw)
	if attached == nil {
		return
	}

	var resource interface{}

	if attachedID == "" {
		resource = api.NewCollectionResource(attached, req, nil)
	} else {
		resource = api.NewItemResource(attached[0], req)
	}

	if writeErr := api.WriteResourceAsJSON(rw, resource); writeErr != nil {
		m.logger.WithError(writeErr).Errorln("failed to write json response")
	}
}

func (m *Manager) getAttachedResourceOrWriteError(client *mcu.ClientResource, attachedID string, rw http.ResponseWriter) []*mcu.AttachedResource {
	attached := client.Attached(attachedID)

	if attachedID != "" {
		if len(attached) != 1 {
			if writeErr := api.WriteErrorAsJSON(rw, api.NewErrorWithCodeAndMessage(
				"ErrorMessageClientTransactionNotfound",
				"The specified transaction is not attached",
				api.ErrNotFound,
			)); writeErr != nil {
				m.logger.WithError(writeErr).Errorln("failed to write json error")
			}
			return nil
		}
	}

	return attached
}

func (m *Manager) HTTPClientsAttachedBridgeHandler(rw http.ResponseWriter, req *http.Request) {
	clientID, _ := api.GetRequestVar(req, "clientID")

	client := m.getClientResourceOrWriteError(clientID, rw)
	if client == nil {
		return
	}

	attachedID, _ := api.GetRequestVar(req, "attachedID")
	attached := m.getAttachedResourceOrWriteError(client, attachedID, rw)
	if attached == nil {
		return
	}

	a := attached[0]

	bridgeID, _ := api.GetRequestVar(req, "bridgeID")
	if a.Bridge != bridgeID {
		if writeErr := api.WriteErrorAsJSON(rw, api.NewErrorWithCodeAndMessage(
			"ErrorMessageBridgeNotfound",
			"The specified bridge is not valid for this transaction",
			api.ErrNotFound,
		)); writeErr != nil {
			m.logger.WithError(writeErr).Errorln("failed to write json error")
		}
		return
	}

	handler := a.BridgeHTTPHandler()
	if handler == nil {
		if writeErr := api.WriteErrorAsJSON(rw, api.NewErrorWithCodeAndMessage(
			"ErrorMessageBridgeUnsupported",
			"The specified bridge does not support this API",
			api.ErrNotFound,
		)); writeErr != nil {
			m.logger.WithError(writeErr).Errorln("failed to write json error")
		}
		return
	}

	// Strip path prefix and let bridge continue.
	idx := strings.Index(req.URL.Path, "/"+bridgeID)
	prefix := req.URL.Path[0 : idx+len(bridgeID)+1]
	http.StripPrefix(prefix, http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "" {
			req.URL.Path = "/"
		}
		handler.ServeHTTP(rw, req)
	})).ServeHTTP(rw, req)
}
