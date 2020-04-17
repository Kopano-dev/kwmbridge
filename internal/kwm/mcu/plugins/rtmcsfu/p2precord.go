/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"github.com/pion/webrtc/v2"
	"github.com/sasha-s/go-deadlock"

	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
)

type P2PRecord struct {
	deadlock.RWMutex

	controller *P2PController

	dataChannel *webrtc.DataChannel

	handshake *kwm.P2PTypeHandshake
}

func NewP2PRecord(controller *P2PController, dataChannel *webrtc.DataChannel) *P2PRecord {
	return &P2PRecord{
		controller: controller,

		dataChannel: dataChannel,
	}
}

func (record *P2PRecord) reset() error {
	record.handshake = nil
	if record.dataChannel != nil {
		if err := record.dataChannel.Close(); err != nil {
			record.controller.logger.WithError(err).Errorln("failed to close data channel of p2p record")
		}
		record.dataChannel = nil
	}

	return nil
}
