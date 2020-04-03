/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/orcaman/concurrent-map"
	"github.com/pion/webrtc/v2"
	"github.com/sirupsen/logrus"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"
)

type P2PController struct {
	sync.RWMutex

	parent *ConnectionRecord
	logger logrus.FieldLogger

	// Holds p2p connection records.
	connections cmap.ConcurrentMap
}

func NewP2PController(parent *ConnectionRecord) *P2PController {
	return &P2PController{
		parent: parent,
		logger: parent.owner.channel.logger,

		connections: cmap.New(),
	}
}

func (controller *P2PController) reset() {
}

func (controller *P2PController) handleClose(pc *PeerConnection, dataChannel *webrtc.DataChannel) error {
	record, exists := controller.connections.Pop(pc.ID())
	if !exists {
		return nil
	}

	p2pRecord := record.(*P2PRecord)

	p2pRecord.Lock()
	defer p2pRecord.Unlock()

	if p2pRecord.dataChannel != dataChannel {
		// Ignore when data channel does not match.
		return nil
	}

	return p2pRecord.reset()
}

func (controller *P2PController) handleData(pc *PeerConnection, dataChannel *webrtc.DataChannel, raw webrtc.DataChannelMessage) {
	message := &p2pMessage{}
	err := json.Unmarshal(raw.Data, message)
	if err != nil {
		controller.logger.WithError(err).Errorln("sfu data channel message parse error")
		return
	}

	controller.logger.Debugln("lll p2p handle data", pc.ID())

	record := controller.connections.Upsert(pc.ID(), nil, func(ok bool, p2pRecord interface{}, n interface{}) interface{} {
		if !ok {
			p2pRecordImpl := NewP2PRecord(controller, dataChannel)
			controller.logger.WithField("pcid", pc.ID()).Debugln("lll new p2p")

			p2pRecord = p2pRecordImpl
		}
		return p2pRecord
	})
	p2pRecord := record.(*P2PRecord)

	switch message.RTMTypeSubtypeEnvelope.Type {
	case "p2p":
		err = controller.handleP2PMessage(message, p2pRecord)

	default:
		controller.logger.WithField("type", message.RTMTypeSubtypeEnvelope.Type).Warnln("sfu received unknown data channel message type")
		return
	}

	if err != nil {
		controller.logger.WithError(err).Errorln("error while processing data channel message")
	}
}

func (controller *P2PController) handleP2PMessage(message *p2pMessage, p2pRecord *P2PRecord) error {
	p2pRecord.Lock()
	defer p2pRecord.Unlock()

	switch message.RTMTypeSubtypeEnvelope.Subtype {
	case "handshake":

		if p2pRecord.handshake != nil {
			controller.logger.Warnln("p2p connection received handshake, but already has handshaked")
			return nil
		}

		p2pRecord.handshake = message.P2PTypeHandshake

		reply := &kwm.P2PTypeHandshake{
			RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
				Type:    "p2p",
				Subtype: "handshake_reply",
			},

			Ts: message.P2PTypeHandshake.Ts,
			V:  message.P2PTypeHandshake.V,
		}
		if err := controller.sendDataChannelPayload(p2pRecord, reply); err != nil {
			return fmt.Errorf("p2p connection failed to send handshake reply: %w", err)
		}

		if len(message.P2PTypeHandshake.Data) > 0 {
			extra := &p2pMessage{}
			err := json.Unmarshal(message.P2PTypeHandshake.Data, extra)
			if err != nil {
				return fmt.Errorf("p2p connection received handshake with invalid data: %w", err)
			}

			if extra.RTMTypeSubtypeEnvelope.Type == "p2p" && extra.RTMTypeSubtypeEnvelope.Subtype != "handshake" {
				err = controller.handleP2PMessage(extra, p2pRecord)
				if err != nil {
					return fmt.Errorf("p2p connection failed to process handshake data: %w", err)
				}
			}
		}

	default:
		controller.logger.WithField("subtype", message.RTMTypeSubtypeEnvelope.Subtype).Warnln("sfu received unknown p2p data channel message sub type")
		break
	}

	return nil
}

func (controller *P2PController) sendDataChannelPayload(p2pRecord *P2PRecord, payload interface{}) error {
	dataChannel := p2pRecord.dataChannel

	payloadBytes, err := json.MarshalIndent(payload, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to marshal channel payload: %w", err)
	}

	controller.logger.Debugln("jjj send data channel payload", string(payloadBytes))

	err = dataChannel.SendText(string(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to send text payload via data channel: %w", err)
	}
	return nil
}
