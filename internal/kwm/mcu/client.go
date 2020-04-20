/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package mcu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/orcaman/concurrent-map"
	"github.com/sirupsen/logrus"
	"nhooyr.io/websocket"

	"stash.kopano.io/kwm/kwmbridge/internal/bpool"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm/utils"
)

const (
	websocketMaxMessageSize = 1048576 // 100 KiB, this is what kwmserver uses.
)

type Client struct {
	mutex sync.RWMutex
	id    string

	c *kwm.Client

	baseURI string

	options *Options
	logger  logrus.FieldLogger

	wsCtx    context.Context
	wsCancel context.CancelFunc
	ws       *websocket.Conn

	attached cmap.ConcurrentMap
}

func NewClient(c *kwm.Client, options *Options) (*Client, error) {
	mcu := &Client{
		c:  c,
		id: utils.NewRandomGUID(),

		baseURI: c.GetBaseURI() + "/api/kwm/v2/mcu",

		attached: cmap.New(),
	}

	if options == nil {
		return nil, errors.New("options cannot be nil")
	}
	mcu.options = options
	mcu.logger = options.Logger

	return mcu, nil
}

func (mcu *Client) ID() string {
	return mcu.id
}

func (mcu *Client) Start(ctx context.Context) error {
	baseURI, err := utils.AsWebsocketURL(mcu.baseURI)
	if err != nil {
		return fmt.Errorf("failed to parse mcu API base URL: %w", err)
	}
	uri := baseURI + "/websocket"

	mcu.mutex.Lock()
	wsCtx, wsCancel := context.WithCancel(ctx)

	// TODO(longsleep): Add timeout via context.

	options := &websocket.DialOptions{
		HTTPClient:   mcu.options.HTTPClient,
		Subprotocols: []string{"kwmmcu-protocol"},
	}
	ws, _, err := websocket.Dial(wsCtx, uri, options)
	if err != nil {
		wsCancel()
		mcu.mutex.Unlock()
		return fmt.Errorf("failed to connect mcu API websocket: %w", err)
	}

	ws.SetReadLimit(websocketMaxMessageSize)

	mcu.logger.Infoln("mcu API connection connection established")
	mcu.ws = ws
	mcu.wsCtx = wsCtx
	mcu.wsCancel = wsCancel
	mcu.mutex.Unlock()

	errCh := make(chan error, 1)

	go func() {
		readPumpErr := mcu.readPump(ctx, ws) // This blocks.
		errCh <- readPumpErr                 // Always send result, to unblock cleanup.
	}()

	err = <-errCh
	wsCancel()

	mcu.mutex.Lock()
	mcu.ws = nil
	mcu.mutex.Unlock()

	return err
}

func (mcu *Client) readPump(ctx context.Context, ws *websocket.Conn) error {
	var mt websocket.MessageType
	var reader io.Reader
	var b *bytes.Buffer
	var err error
	for {
		mt, reader, err = ws.Reader(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			switch websocket.CloseStatus(err) {
			case websocket.StatusNormalClosure, websocket.StatusGoingAway:
				mcu.logger.WithField("status_code", websocket.CloseStatus(err)).Debugln("mcu API connection close")
				return nil
			}
			mcu.logger.WithError(err).Errorln("mcu API connection failed to get reader")
			return err
		}

		b = bpool.Get()
		if _, err = b.ReadFrom(reader); err != nil {
			bpool.Put(b)
			return err
		}

		switch mt {
		case websocket.MessageText:
		default:
			mcu.logger.WithField("message_type", mt).Warnln("mcu API connection received unknown websocket message type")
			continue
		}

		message := &kwm.MCUTypeContainer{}
		err = json.Unmarshal(b.Bytes(), message)
		bpool.Put(b)
		if err != nil {
			mcu.logger.WithError(err).Errorln("mcu API connection websocket message parse error")
			continue
		}

		switch message.Type {
		case "attach", "detach":
			err = mcu.handleWebsocketMessage(ctx, message)

		default:
			mcu.logger.WithField("type", message.Type).Warnln("mcu API connection received unknown mcu message type")
			continue
		}

		if err != nil {
			mcu.logger.WithError(err).Errorln("error while processing mcu API connection websocket message")
			return err
		}
	}
}

func (mcu *Client) handleWebsocketMessage(ctx context.Context, message *kwm.MCUTypeContainer) error {
	//mcu.logger.Debugln("xxx known message", message.Type, message.Transaction)

	switch message.Type {
	case "attach":
		logger := mcu.logger.WithFields(logrus.Fields{
			"transaction": message.Transaction,
			"plugin":      message.Plugin,
		})
		logger.Debugln("attaching mcu API transaction to plugin")

		// Create new ws connection to attach to channel at mcu.
		baseURI, err := utils.AsWebsocketURL(mcu.baseURI)
		if err != nil {
			return fmt.Errorf("failed to parse mcu API base URL: %w", err)
		}

		uri := baseURI + "/websocket/" + message.Transaction

		// TODO(longsleep): Add timeout via context.

		options := &websocket.DialOptions{
			HTTPClient:   mcu.options.HTTPClient,
			Subprotocols: []string{"kwmmcu-protocol"},
		}
		ws, _, err := websocket.Dial(ctx, uri, options)
		if err != nil {
			return fmt.Errorf("failed to connect mcu API transaction websocket: %w", err)
		}

		ws.SetReadLimit(websocketMaxMessageSize)

		factoryFunc := mcu.options.AttachPluginFactoryFunc
		if factoryFunc == nil {
			return fmt.Errorf("failed to attach mcu API plugin, no factory in mcu options")
		}

		plugin, err := factoryFunc(message, ws, mcu.options)
		if err != nil {
			return fmt.Errorf("failed to create mcu API plugin from attach factory: %w", err)
		}

		mcu.attached.Set(message.Transaction, &AttachedRecord{
			when:    time.Now(),
			plugin:  plugin,
			message: message,
		})
		go func() {
			defer func() {
				// Cleanup.
				if _, exists := mcu.attached.Pop(message.Transaction); exists {
					logger.WithField("attached_count", mcu.attached.Count()).Infoln("detached plugin from mcu API transaction")
				}
			}()

			// Start plugin, this blocks.
			pluginErr := plugin.Start(ctx)
			if pluginErr != nil {
				logger.WithError(pluginErr).Errorln("mcu API plugin exit with error")
			} else {
				logger.Infoln("mcu API transaction plugin ended")
			}
			// Close plugin when it ended.
			closeErr := plugin.Close()
			if closeErr != nil {
				logger.WithError(closeErr).Errorln("mcu API plugin close error")
			}
		}()
		logger.WithField("attached_count", mcu.attached.Count()).Infoln("attached mcu API transaction to plugin")

	case "detatch":
		logger := mcu.logger.WithFields(logrus.Fields{
			"transaction": message.Transaction,
		})
		logger.Infoln("detatching plugin from mcu API transaction")

		record, exists := mcu.attached.Pop(message.Transaction)
		if exists {
			attachedRecord := record.(*AttachedRecord)
			closeErr := attachedRecord.plugin.Close()
			if closeErr != nil {
				logger.WithError(closeErr).Errorln("mcu API plugin close error on detatch")
			}
			logger.Infoln("detached plugin from mcu API transaction")
		}

	}

	return nil
}

func (mcu *Client) ping() error {
	mcu.mutex.RLock()
	ws := mcu.ws
	wsCtx := mcu.wsCtx
	mcu.mutex.RUnlock()

	if ws == nil {
		return errors.New("no connection")
	}

	ctx, cancel := context.WithTimeout(wsCtx, 10*time.Second)
	defer cancel()

	err := ws.Ping(ctx)
	if err != nil {
		return fmt.Errorf("failed to communicate with mcu API websocket: %w", err)
	}
	return nil
}

func (mcu *Client) Close() error {
	mcu.mutex.Lock()
	defer mcu.mutex.Unlock()

	if mcu.wsCancel != nil {
		mcu.wsCancel()
	}
	return nil
}

func (mcu *Client) Resource() *ClientResource {
	mcu.mutex.RLock()
	defer mcu.mutex.RUnlock()

	return &ClientResource{
		client: mcu,

		ID:            mcu.id,
		URI:           mcu.baseURI,
		Connected:     mcu.ws != nil,
		AttachedCount: mcu.attached.Count(),
	}
}
