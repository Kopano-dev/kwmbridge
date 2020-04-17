/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"github.com/pion/webrtc/v2"
)

type TrackRecord struct {
	track       *webrtc.Track
	source      *UserRecord
	connection  *ConnectionRecord
	remove      bool
	transceiver bool
	rtcpCh      chan *RTCPRecord // Holds the track sender's rtcpCh.
}
