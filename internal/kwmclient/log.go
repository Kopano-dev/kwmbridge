/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package kwmclient

import (
	pionLogging "github.com/pion/logging"
	"github.com/sirupsen/logrus"
)

type leveledLogrusLogger struct {
	logrus.FieldLogger
}

func (ll *leveledLogrusLogger) Debug(msg string) {
	//ll.FieldLogger.Debug(msg)
}
func (ll *leveledLogrusLogger) Debugf(format string, args ...interface{}) {
	//ll.FieldLogger.Debugf(format, args...)
}
func (ll *leveledLogrusLogger) Error(msg string) {
	ll.FieldLogger.Error(msg)
}
func (ll *leveledLogrusLogger) Info(msg string) {
	ll.FieldLogger.Info(msg)
}
func (ll *leveledLogrusLogger) Trace(msg string) {
	//ll.FieldLogger.Debug(msg)
}
func (ll *leveledLogrusLogger) Tracef(format string, args ...interface{}) {
	//ll.FieldLogger.Debugf(format, args...)
}
func (ll *leveledLogrusLogger) Warn(msg string) {
	ll.FieldLogger.Warn(msg)
}

type loggerFactory struct {
	logger logrus.FieldLogger
}

func (factory *loggerFactory) NewLogger(scope string) pionLogging.LeveledLogger {
	return &leveledLogrusLogger{factory.logger.WithField("webrtc", scope)}
}
