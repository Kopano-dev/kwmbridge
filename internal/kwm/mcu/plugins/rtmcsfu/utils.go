/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"strings"

	"github.com/pion/webrtc/v2"
)

func remoteSDPTransform(sessionDescription *webrtc.SessionDescription) error {
	sdpLinesIn := strings.Split(sessionDescription.SDP, "\r\n")
	sdpLinesOut := sdpLinesIn[:0]
	for _, line := range sdpLinesIn {
		if strings.HasPrefix(line, "b=TIAS:") {
			// b=TIAS is unsupported, filter out. Used by Firefox for bandwidth control.
			continue
		}

		sdpLinesOut = append(sdpLinesOut, line)
	}

	sessionDescription.SDP = strings.Join(sdpLinesOut, "\r\n")
	return nil
}

func localSDPTransform(sessionDescription *webrtc.SessionDescription) error {
	return nil
}
