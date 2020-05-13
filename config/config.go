/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package config

import (
	"net/http"
	"net/url"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

// Config defines a Server's configuration settings.
type Config struct {
	ListenAddr string

	WithMetrics       bool
	MetricsListenAddr string

	HTTPClient *http.Client

	Iss *url.URL

	Logger logrus.FieldLogger

	Metrics prometheus.Registerer
	Survey  prometheus.Registerer

	KWMServerURIs []*url.URL

	ICEInterfaces            []string
	ICENetworkTypes          []string
	ICEEphemeralUDPPortRange [2]uint16
	ICELite                  bool
}
