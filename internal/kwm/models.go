/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package kwm

import (
	"encoding/json"

	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"
)

// TODO(longsleep): Import these from kwmserver, it is currently is there in the
// signaling/mcu package, but really should move to the api package. Until it
// becomes available there, we clone it here.

// MCUTypeContainer is the container for basic mcu websocket messages.
type MCUTypeContainer struct {
	Type        string `json:"type"`
	Transaction string `json:"transaction"`
	Plugin      string `json:"plugin"`
	Handle      int64  `json:"handleID"`
}

type RTMDataWebRTCSignal struct {
	Renegotiate bool `json:"renegotiate,omitempty"`
	Noop        bool `json:"noop,omitempty"`

	Type               json.RawMessage `json:"type,omitempty"`
	SDP                json.RawMessage `json:"sdp,omitempty"`
	Candidate          json.RawMessage `json:"candidate,omitempty"`
	TransceiverRequest json.RawMessage `json:"transceiverRequest,omitempty"`
}

type RTMDataTransceiverRequest struct {
	Kind string                         `json:"kind"`
	Init *RTMDataTransceiverRequestInit `json:"init,omitempty"`
}

type RTMDataTransceiverRequestInit struct {
	Direction string `json:"direction,omitempty"`
}

type P2PTypeP2P struct {
	*api.RTMTypeSubtypeEnvelope

	Version uint64 `json:"v"`
	Ts      uint64 `json:"ts"`

	Data    json.RawMessage  `json:"data,omitempty"`
	Streams []*P2PDataStream `json:"streams"`
}

type P2PDataStream struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Token   string `json:"token"`
	Version uint64 `json:"v"`
}
