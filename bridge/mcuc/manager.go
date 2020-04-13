/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package mcuc

import (
	"context"
	"errors"
	"fmt"
	"github.com/sirupsen/logrus"
	"net/url"
	"sync"
	"time"

	cfg "stash.kopano.io/kwm/kwmbridge/config"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm/mcu"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm/mcu/plugins/rtmcsfu"
)

// Manager handles MCU clients.
type Manager struct {
	logger logrus.FieldLogger
	ctx    context.Context
	config *cfg.Config

	wg      sync.WaitGroup
	clients []*mcu.Client
}

func NewManager(ctx context.Context, config *cfg.Config, uris []*url.URL) (*Manager, error) {
	m := &Manager{
		logger: config.Logger.WithField("manager", "mcuc"),
		ctx:    ctx,
		config: config,
	}

	for _, uri := range uris {
		if mcuc, err := m.connect(uri); err != nil {
			return nil, fmt.Errorf("failed to connect: %w", err)
		} else {
			m.clients = append(m.clients, mcuc)
		}
	}

	return m, nil
}

func (m *Manager) connect(uri *url.URL) (*mcu.Client, error) {
	logger := m.logger
	ctx := m.ctx
	config := m.config

	logger.WithField("url", uri).Infoln("creating kwm client")
	kwmc, err := kwm.NewClient(uri, &kwm.Config{
		Config: config,

		HTTPClient: config.HTTPClient,
		Logger:     config.Logger.WithField("url", uri),
	})
	if err != nil {
		return nil, err
	}

	mcuc, err := mcu.NewClient(kwmc, &mcu.Options{
		Config: config,

		HTTPClient: config.HTTPClient,
		Logger:     m.logger.WithField("url", uri),

		AttachPluginFactoryFunc: rtmcsfu.New,
	})
	if err != nil {
		return nil, err
	}

	m.wg.Add(1)
	go func() {
		defer func() {
			logger.Debugln("kwm mcu connector stopped")
			m.wg.Done()
		}()
		for {
			logger.WithField("url", uri).Infoln("connecting to kwm mcu API")
			err = mcuc.Start(ctx) // Connect and reconnect, this blocks.
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.WithError(err).Warnln("kwm mcu API connection stopped with error, restart scheduled")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
				logger.Infoln("reconnecting to kwm mcu API")
				// breaks and continues.
			}
		}
	}()
	return mcuc, nil
}

func (m *Manager) Wait() {
	m.wg.Wait()
}

func (m *Manager) NumActive() uint64 {
	// TODO(longsleep): Return sum of active connections for each client.
	return 0
}
