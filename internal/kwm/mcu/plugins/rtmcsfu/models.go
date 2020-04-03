/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"

	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
)

type webrtcMessage struct {
	*api.RTMTypeEnvelope
	*api.RTMTypeWebRTC
}

type p2pMessage struct {
	*api.RTMTypeSubtypeEnvelope
	*kwm.P2PTypeHandshake
}
