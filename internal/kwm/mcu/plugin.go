/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package mcu

import (
	"context"

	"nhooyr.io/websocket"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
)

type Plugin interface {
	Start(context.Context) error
	Close() error
}

type AttachPluginFactoryFunc func(attach *kwm.WebsocketMessage, ws *websocket.Conn, options *Options) (Plugin, error)
