/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package kwm

import (
	"errors"
	"net/url"

	"github.com/sirupsen/logrus"
)

type Client struct {
	uri *url.URL

	config *Config
	logger logrus.FieldLogger
}

func NewClient(uri *url.URL, cfg *Config) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("config cannot be nil")
	}

	c := &Client{
		config: cfg,
		logger: cfg.Logger,
	}
	err := c.init(uri)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Client) GetBaseURI() string {
	return c.uri.String()
}

func (c *Client) init(uri *url.URL) error {
	c.uri = uri
	switch uri.Scheme {
	case "https":
	case "http":
	default:
		return errors.New("unknown URI scheme")
	}

	return nil
}
