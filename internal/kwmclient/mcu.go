/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package kwmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/orcaman/concurrent-map"
	"github.com/sirupsen/logrus"
	"nhooyr.io/websocket"

	"stash.kopano.io/kwm/kwmbridge/config"
	"stash.kopano.io/kwm/kwmbridge/internal/bpool"
)

type MCU interface {
	Start(context.Context) error
	Close() error
}

type MCUPlugin interface {
	Start(context.Context) error
	Close() error
}

type mcuClient struct {
	c *KWMClient

	baseURI string

	options *MCUOptions
	logger  logrus.FieldLogger

	wsCtx    context.Context
	wsCancel context.CancelFunc
	ws       *websocket.Conn

	attached cmap.ConcurrentMap
}

type attachedRecord struct {
	sync.Mutex
	when   time.Time
	plugin MCUPlugin
}

func NewMCU(c *KWMClient, options *MCUOptions) (MCU, error) {
	mcu := &mcuClient{
		c: c,

		baseURI: c.uri.String() + "/api/kwm/v2/mcu",

		attached: cmap.New(),
	}

	if options == nil {
		return nil, errors.New("options cannot be nil")
	}
	mcu.options = options
	mcu.logger = options.Logger

	return mcu, nil
}

type MCUOptions struct {
	Config *config.Config

	Logger     logrus.FieldLogger
	HTTPClient *http.Client
}

func (mcu *mcuClient) Start(ctx context.Context) error {
	baseURI, err := asWebsocketURL(mcu.baseURI)
	if err != nil {
		return fmt.Errorf("failed to parse mcu base URL: %w", err)
	}
	uri := baseURI + "/websocket"

	mcu.wsCtx, mcu.wsCancel = context.WithCancel(ctx)
	// TODO(longsleep): Add timeout via context.

	options := &websocket.DialOptions{
		HTTPClient:   mcu.options.HTTPClient,
		Subprotocols: []string{"kwmmcu-protocol"},
	}
	ws, _, err := websocket.Dial(mcu.wsCtx, uri, options)
	if err != nil {
		return fmt.Errorf("failed to connect mcu websocket: %w", err)
	}

	mcu.ws = ws

	errCh := make(chan error, 1)

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()

		mcu.logger.Infoln("mcu connection established, waiting for action")
		readPumpErr := mcu.readPump()
		if readPumpErr != nil {
			errCh <- readPumpErr
		}
	}()

	select {
	case err = <-errCh:
		// breaks
	}

	mcu.wsCancel()
	wg.Wait()

	return err
}

func (mcu *mcuClient) readPump() error {
	var mt websocket.MessageType
	var reader io.Reader
	var b *bytes.Buffer
	var err error
	for {
		mt, reader, err = mcu.ws.Reader(mcu.wsCtx)
		if err != nil {
			mcu.logger.WithError(err).Errorln("mcu failed to get reader")
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
			mcu.logger.WithField("message_type", mt).Warnln("mcu received unknown websocket message type")
			continue
		}

		message := &mcuMessage{}
		err = json.Unmarshal(b.Bytes(), message)
		bpool.Put(b)
		if err != nil {
			mcu.logger.WithError(err).Errorln("mcu websocket message parse error")
			continue
		}

		switch message.Type {
		case "attach":
			err = mcu.handleWebsocketMessage(message.WebsocketMessage)

		default:
			mcu.logger.WithField("type", message.Type).Warnln("mcu received unknown mcu message type")
			continue
		}

		if err != nil {
			mcu.logger.WithError(err).Errorln("error while processing mcu websocket message")
			return err
		}
	}
}

func (mcu *mcuClient) handleWebsocketMessage(message *WebsocketMessage) error {
	mcu.logger.Debugln("xxx known message", message.Type, message.Transaction)

	switch message.Type {
	case "attach":
		logger := mcu.logger.WithFields(logrus.Fields{
			"transaction": message.Transaction,
			"plugin":      message.Plugin,
		})
		logger.Infoln("attaching mcu transaction to plugin")

		// Create new ws connection to attach to channel at mcu.
		baseURI, err := asWebsocketURL(mcu.baseURI)
		if err != nil {
			return fmt.Errorf("failed to parse mcu base URL: %w", err)
		}

		uri := baseURI + "/websocket/" + message.Transaction

		// TODO(longsleep): Add timeout via context.

		options := &websocket.DialOptions{
			HTTPClient:   mcu.options.HTTPClient,
			Subprotocols: []string{"kwmmcu-protocol"},
		}
		ws, _, err := websocket.Dial(mcu.wsCtx, uri, options)
		if err != nil {
			return fmt.Errorf("failed to connect mcu transaction websocket: %w", err)
		}

		//ws.SetReadLimit(32768 * 2) // NOTE(longsleep): Why is stuff so big, 32k should be enough?

		plugin, err := NewRTMChannelSFU(message, ws, mcu.options)
		if err != nil {
			return fmt.Errorf("failed to create mcu plugin: %w", err)
		}

		mcu.attached.Set(message.Transaction, &attachedRecord{
			when:   time.Now(),
			plugin: plugin,
		})
		go func() {
			pluginErr := plugin.Start(mcu.wsCtx)
			if pluginErr != nil {
				logger.WithError(pluginErr).Errorln("mcu plugin exit with error")
			} else {
				logger.Infoln("msc transaction plugin end")
			}
			closeErr := plugin.Close()
			if closeErr != nil {
				logger.WithError(closeErr).Errorln("mcu plugin close error")
			}
			mcu.attached.Remove(message.Transaction)
		}()
	}

	return nil
}

func (mcu *mcuClient) ping() error {
	ctx, cancel := context.WithTimeout(mcu.wsCtx, 10*time.Second)
	defer cancel()

	err := mcu.ws.Ping(ctx)
	if err != nil {
		return fmt.Errorf("failed to communicate with mcu websocket: %w", err)
	}
	return nil
}

func (mcu *mcuClient) Close() error {
	mcu.wsCancel()
	return nil
}
