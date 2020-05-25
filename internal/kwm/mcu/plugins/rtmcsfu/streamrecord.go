/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"github.com/sasha-s/go-deadlock"
)

type StreamRecord struct {
	deadlock.RWMutex

	controller *P2PController
	connection *P2PRecord

	id    string
	kind  string
	token string
}

func NewStreamRecord(controller *P2PController) *StreamRecord {
	return &StreamRecord{
		controller: controller,
	}
}

func (record *StreamRecord) reset() error {
	record.connection = nil
	return nil
}
