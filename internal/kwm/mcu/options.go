/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package mcu

import (
	"net/http"

	"github.com/sirupsen/logrus"

	"stash.kopano.io/kwm/kwmbridge/config"
)

type Options struct {
	Config *config.Config

	Logger     logrus.FieldLogger
	HTTPClient *http.Client

	AttachPluginFactoryFunc AttachPluginFactoryFunc
}
