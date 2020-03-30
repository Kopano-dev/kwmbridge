/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v2"
)

type RTCPRecord struct {
	packet rtcp.Packet
	track  *webrtc.Track
}
