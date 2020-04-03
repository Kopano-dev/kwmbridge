/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"strconv"
	"sync/atomic"

	"github.com/pion/webrtc/v2"
)

var peerConnectionIDcounter uint64 = 0

type PeerConnection struct {
	*webrtc.PeerConnection

	id string
}

func NewPeerConnection(api *webrtc.API, configuration *webrtc.Configuration) (*PeerConnection, error) {
	pc, err := api.NewPeerConnection(*configuration)
	if err != nil {
		return nil, err
	}

	id := atomic.AddUint64(&peerConnectionIDcounter, 1)

	return &PeerConnection{
		PeerConnection: pc,

		id: strconv.FormatUint(id, 10),
	}, nil
}

func (pc *PeerConnection) ID() string {
	return pc.id
}
