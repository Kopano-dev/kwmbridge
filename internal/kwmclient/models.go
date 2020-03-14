/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package kwmclient

import (
	"encoding/json"

	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"
)

// TODO(longsleep): Import this from kwmserver, it is currently is there in the
// signaling/mcu package, but really should move to the api package. Until it
// becomes available there, we clone it here.

type mcuMessage struct {
	*WebsocketMessage
}

// WebsocketMessage is the container for basic mcu websocket messages.
type WebsocketMessage struct {
	Type        string `json:"type"`
	Transaction string `json:"transaction"`
	Plugin      string `json:"plugin"`
	Handle      int64  `json:"handle_id"`
}

type webrtcMessage struct {
	*api.RTMTypeEnvelope
	*api.RTMTypeWebRTC
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
	Kind string `json:"kind"`
	// TODO(longsleep): Add Init object (might not be needed?)
}
