/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package kwmclient

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/sirupsen/logrus"

	"stash.kopano.io/kwm/kwmbridge/config"
)

type KWMClient struct {
	uri *url.URL
	tls bool

	config *Config
	logger logrus.FieldLogger

	MCU MCU
}

type Config struct {
	Config *config.Config

	Logger     logrus.FieldLogger
	HTTPClient *http.Client
}

func New(uri *url.URL, cfg *Config) (*KWMClient, error) {
	if cfg == nil {
		return nil, errors.New("config cannot be nil")
	}

	c := &KWMClient{
		config: cfg,
		logger: cfg.Logger,
	}
	err := c.init(uri)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *KWMClient) init(uri *url.URL) error {
	c.uri = uri
	switch uri.Scheme {
	case "https":
		c.tls = true
	case "http":
	default:
		return errors.New("unknown URI scheme")
	}

	mcu, err := NewMCU(c, &MCUOptions{
		Config: c.config.Config,

		Logger:     c.config.Logger,
		HTTPClient: c.config.HTTPClient,
	})
	if err != nil {
		return fmt.Errorf("failed to create MCU: %w", err)
	}
	c.MCU = mcu

	return nil
}
