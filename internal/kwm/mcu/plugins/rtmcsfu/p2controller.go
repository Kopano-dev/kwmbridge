/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/orcaman/concurrent-map"
	"github.com/pion/webrtc/v2"
	"github.com/sasha-s/go-deadlock"
	"github.com/sirupsen/logrus"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"
)

type P2PController struct {
	deadlock.RWMutex

	parent *ConnectionRecord
	logger logrus.FieldLogger

	handshake   *kwm.P2PTypeP2P
	dataChannel *webrtc.DataChannel

	callbacks cmap.ConcurrentMap // Hols p2p connection records, by token.

	streams map[string]*StreamRecord // Holds stream records, by id
	pending map[string]*StreamRecord // Holds stream records, by id
}

func NewP2PController(parent *ConnectionRecord) *P2PController {
	controller := &P2PController{
		parent: parent,
		logger: parent.owner.channel.logger,

		callbacks: cmap.New(),
		streams:   make(map[string]*StreamRecord),
		pending:   make(map[string]*StreamRecord),
	}

	return controller
}

func (controller *P2PController) bind(dataChannel *webrtc.DataChannel) error {
	controller.dataChannel = dataChannel

	var added []*StreamRecord
	for _, stream := range controller.pending {
		added = append(added, stream)
	}
	controller.pending = make(map[string]*StreamRecord)
	go func() {
		controller.Lock()
		defer controller.Unlock()
		if controller.dataChannel == nil || controller.dataChannel != dataChannel {
			// Datachannel has changed, do nothing.
			return
		}

		err := controller.announceStreams(added, false)
		if err != nil {
			controller.logger.WithError(err).Errorln("qqq jjj p2p faild to announce pending streams on bind")
		}
	}()

	return nil
}

func (controller *P2PController) reset() {
	controller.handshake = nil
	controller.dataChannel = nil

	controller.callbacks = cmap.New()
	controller.streams = make(map[string]*StreamRecord)
	controller.pending = make(map[string]*StreamRecord)
}

func (controller *P2PController) handleData(dataChannel *webrtc.DataChannel, raw webrtc.DataChannelMessage) {
	controller.RLock()
	if controller.dataChannel != dataChannel {
		// Ignore when data channel is changed.
		controller.RUnlock()
		return
	}
	controller.RUnlock()

	message := &p2pMessage{}
	err := json.Unmarshal(raw.Data, message)
	if err != nil {
		controller.logger.WithError(err).Errorln("sfu data channel message parse error")
		return
	}

	//controller.logger.Debugln("lll p2p handle data", dataChannel.ID(), message.RTMTypeEnvelope.Type)

	switch message.RTMTypeEnvelope.Type {
	case "p2p":
		m := &kwm.P2PTypeP2P{}
		if unmarshalErr := json.Unmarshal(raw.Data, m); unmarshalErr != nil {
			controller.logger.WithError(unmarshalErr).Errorln("sfu data channel p2p message parse error")
			return
		}

		controller.Lock()
		err = controller.handleP2PMessage(m)
		controller.Unlock()

	case "webrtc":
		m := &api.RTMTypeWebRTC{}
		if unmarshalErr := json.Unmarshal(raw.Data, m); unmarshalErr != nil {
			controller.logger.WithError(unmarshalErr).Errorln("sfu data channel p2p message parse error")
			return
		}

		err = controller.handleWebRTCMessage(m)

	default:
		controller.logger.WithField("type", message.RTMTypeEnvelope.Type).Warnln("sfu received unknown data channel message type")
		return
	}

	if err != nil {
		controller.logger.WithError(err).Errorln("error while processing data channel message")
	}
}

func (controller *P2PController) handleP2PMessage(message *kwm.P2PTypeP2P) error {
	//controller.logger.Debugln("lll p2p handle p2p", message, message.Version)

	switch message.RTMTypeSubtypeEnvelope.Subtype {
	case "handshake":

		if controller.handshake != nil {
			controller.logger.Warnln("p2p connection received handshake, but already has handshaked")
			return nil
		}
		controller.handshake = message

		reply := &kwm.P2PTypeP2P{
			RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
				Type:    "p2p",
				Subtype: "handshake_reply",
			},

			Ts:      message.Ts,
			Version: message.Version,
		}
		if err := controller.sendDataChannelPayload(reply); err != nil {
			return fmt.Errorf("p2p connection failed to send handshake reply: %w", err)
		}

		if len(message.Data) > 0 {
			extra := &kwm.P2PTypeP2P{}
			err := json.Unmarshal(message.Data, extra)
			if err != nil {
				return fmt.Errorf("p2p connection received handshake with invalid data: %w", err)
			}

			if extra.RTMTypeSubtypeEnvelope.Type == "p2p" && extra.RTMTypeSubtypeEnvelope.Subtype != "handshake" {
				err = controller.handleP2PMessage(extra)
				if err != nil {
					return fmt.Errorf("p2p connection failed to process handshake data: %w", err)
				}
			}
		}

	case "announce_streams":
		if err := controller.handleAnnounceStreams(message); err != nil {
			return fmt.Errorf("p2p failed to process announce streams: %w", err)
		}

	default:
		controller.logger.WithField("subtype", message.RTMTypeSubtypeEnvelope.Subtype).Warnln("sfu received unknown p2p data channel message sub type")
		break
	}

	return nil
}

func (controller *P2PController) handleAnnounceStreams(message *kwm.P2PTypeP2P) error {
	status := make(map[string]bool)
	added := []*StreamRecord{}
	removed := []*StreamRecord{}

	channel := controller.parent.owner.channel

	for _, streamAnnouncement := range message.Streams {
		if streamRecord, found := controller.streams[streamAnnouncement.ID]; found {
			// Already have this stream.
			if streamRecord.kind != streamAnnouncement.Kind {
				// Ignore changes of stream kind.
				continue
			}
			if streamRecord.token != streamAnnouncement.Token {
				// Update token.
				// TODO(longsleep): Remove/re add callback
				// TODO(longsleep): locking
				streamRecord.token = streamAnnouncement.Token
				streamRecord.reset()
			}
			status[streamAnnouncement.ID] = false
		} else {
			// New stream
			status[streamAnnouncement.ID] = true
			streamRecordImpl := NewStreamRecord(controller)
			streamRecordImpl.id = streamAnnouncement.ID
			streamRecordImpl.kind = streamAnnouncement.Kind
			streamRecordImpl.token = streamAnnouncement.Token
			added = append(added, streamRecordImpl)
		}
	}

	for id, streamRecord := range controller.streams {
		if _, found := status[id]; !found {
			delete(controller.streams, id)
			removed = append(removed, streamRecord)
			// Remove connections for remove streams.
			streamRecord.reset()
		}
	}

	if len(added) == 0 && len(removed) == 0 {
		// Nothing further todo.
		return nil
	}

	controller.logger.Debugln("qqq lll p2p streams have changed", len(added), len(removed), controller.parent.initiator)

	source := controller.parent.owner.id
	logger := controller.logger.WithField("source", source)

	for _, streamRecord := range added {
		// Create record which receives the stream (incoming to sfu)
		p2pRecord := NewP2PRecord(controller.parent.ctx, controller, controller.parent, controller.dataChannel, streamRecord.id, streamRecord.token, nil)
		logger = logger.WithField("id", streamRecord.id)
		logger.WithField("id", streamRecord.id).Debugln("qqq lll new p2p")
		streamRecord.connection = p2pRecord
		controller.streams[streamRecord.id] = streamRecord
		if set := controller.callbacks.SetIfAbsent(streamRecord.token, p2pRecord); !set {
			controller.logger.Warnln("qqq lll p2p ignored stream announcement, token already set")
			continue
		}
		// Negotiate the record which get sent to (outgoing from sfu)
		if err := p2pRecord.negotiate(true); err != nil {
			controller.logger.WithError(err).Debugln("qqq lll p2p failed to trigger negotiate")
			continue
		}
	}

	// Go through all connected channels, and announce stream to other peers.
	for item := range channel.connections.IterBuffered() {
		func() {
			target := item.Key
			record := item.Val

			logger.Debugln("qqq lll p2p sfu selecting", target, source)
			if target == source {
				// Do not publish to self.
				return
			}

			targetRecord := record.(*UserRecord)
			if targetRecord.isClosed() {
				// Do not publish to closed.
				return
			}

			l := logger.WithField("target", target)

			l.Debugln("qqq lll p2p sfu stream announce target")

			var ok bool
			record, ok = targetRecord.connections.Get(source)
			if !ok {
				l.Warnln("qqq lll p2p sfu updating announce target does not have source connection")
				return
			}
			connectionRecord := record.(*ConnectionRecord)
			l.Debugln("qqq lll p2p sfu using connection", connectionRecord.id)

			targetController := connectionRecord.p2p

			if announceErr := targetController.announceStreams(added, true); announceErr != nil {
				l.WithError(announceErr).Errorln("qqq lll p2p announce stream failed")
				return
			}
		}()
	}

	return nil
}

func (controller *P2PController) handleWebRTCMessage(message *api.RTMTypeWebRTC) error {
	// TODO(longsleep): Compare message `v` field with our implementation version.
	var err error

	switch message.Subtype {
	case api.RTMSubtypeNameWebRTCSignal:
		record, _ := controller.callbacks.Get(message.Source)
		if record == nil {
			controller.logger.WithField("source", message.Source).Warnln("lll p2p got signal but has no callback")
			return errors.New("no callback")
		}
		p2pRecord := record.(*P2PRecord)
		if signalErr := p2pRecord.handleWebRTCSignalMessage(message); signalErr != nil {
			controller.logger.WithError(signalErr).WithField("source", message.Source).Warnln("lll p2p error while signal processing")
		}

	default:
		controller.logger.WithField("subtype", message.Subtype).Warnln("lll p2p received unknown webrtc message sub type")
	}

	return err
}

func (controller *P2PController) announceStreams(streams []*StreamRecord, force bool) error {
	announce := make([]*kwm.P2PDataStream, 0)

	// Create record which sends out the stream (outgoing from sfu)
	for _, streamRecord := range streams {
		if controller.dataChannel == nil {
			// Add as pending when no data channel exists yet.
			controller.pending[streamRecord.id] = streamRecord
			continue
		}

		p2pRecord := NewP2PRecord(controller.parent.ctx, controller, controller.parent, controller.dataChannel, streamRecord.id, streamRecord.token, streamRecord.connection)
		controller.logger.WithField("id", streamRecord.id).Debugln("lll p2p sfu new announce stream")
		if set := controller.callbacks.SetIfAbsent(streamRecord.token, p2pRecord); !set {
			controller.logger.Warnln("lll p2p sfu ignored stream announcement, token already set")
			continue
		}

		announce = append(announce, &kwm.P2PDataStream{
			ID:      streamRecord.id,
			Kind:    streamRecord.kind,
			Token:   streamRecord.token,
			Version: 1, // TODO(longsleep): Move to constant.
		})
	}

	if !force && len(announce) == 0 {
		return nil
	}

	// Send out announcement.
	message := &kwm.P2PTypeP2P{
		RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
			Type:    "p2p",
			Subtype: "announce_streams",
		},

		Version: WebRTCPayloadVersion,
		Streams: announce,
	}
	return controller.sendDataChannelPayload(message)
}

func (controller *P2PController) sendDataChannelPayload(payload interface{}) error {
	dataChannel := controller.dataChannel

	payloadBytes, err := json.MarshalIndent(payload, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to marshal channel payload: %w", err)
	}

	//controller.logger.WithField("message", string(payloadBytes)).Debugln("jjj send data channel payload")
	err = dataChannel.SendText(string(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to send text payload via data channel: %w", err)
	}
	return nil
}
