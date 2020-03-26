/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package kwm

import (
	"net/http"

	"github.com/sirupsen/logrus"

	"stash.kopano.io/kwm/kwmbridge/config"
)

type Config struct {
	Config *config.Config

	Logger     logrus.FieldLogger
	HTTPClient *http.Client
}
