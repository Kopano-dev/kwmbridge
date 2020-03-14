/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package server

import (
	"github.com/sirupsen/logrus"
)

type debugLogger struct {
	logger logrus.FieldLogger
	prefix string
}

func (l *debugLogger) Printf(format string, args ...interface{}) {
	l.logger.Debugf(l.prefix+format, args...)
}
