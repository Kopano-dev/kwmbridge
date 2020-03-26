/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package kwm

import (
	"encoding/json"
)

// TODO(longsleep): Import these from kwmserver, it is currently is there in the
// signaling/mcu package, but really should move to the api package. Until it
// becomes available there, we clone it here.

// WebsocketMessage is the container for basic mcu websocket messages.
type WebsocketMessage struct {
	Type        string `json:"type"`
	Transaction string `json:"transaction"`
	Plugin      string `json:"plugin"`
	Handle      int64  `json:"handle_id"`
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
