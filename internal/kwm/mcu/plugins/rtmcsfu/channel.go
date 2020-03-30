/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/orcaman/concurrent-map"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v2"
	"github.com/sirupsen/logrus"
	"nhooyr.io/websocket"
	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"

	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm/utils"
)

type Channel struct {
	sync.RWMutex

	sfu *RTMChannelSFU

	when time.Time

	logger logrus.FieldLogger

	channel  string
	hash     string
	group    string
	pipeline *api.RTMDataWebRTCChannelPipeline

	// Holds incoming connections by user.
	connections cmap.ConcurrentMap

	trackCh    chan *TrackRecord
	receiverCh chan *ConnectionRecord
}

func NewChannel(sfu *RTMChannelSFU, message *api.RTMTypeWebRTC) (*Channel, error) {
	extra := &api.RTMDataWebRTCChannelExtra{}
	err := json.Unmarshal(message.Data, extra)
	if err != nil {
		return nil, fmt.Errorf("failed to parse channel data: %w", err)
	}

	if extra.Pipeline == nil {
		return nil, fmt.Errorf("no pipeline attached channel data, this is unsupported")
	}
	if extra.Pipeline.Mode != "mcu-forward" {
		return nil, fmt.Errorf("unsupported pipeline mode in channel data: %v", extra.Pipeline.Mode)
	}

	channel := &Channel{
		when: time.Now(),
		sfu:  sfu,

		logger: sfu.logger.WithField("channel", message.Channel),

		channel:  message.Channel,
		hash:     message.Hash,
		group:    message.Group,
		pipeline: extra.Pipeline,

		connections: cmap.New(),

		trackCh:    make(chan *TrackRecord, maxChSize),
		receiverCh: make(chan *ConnectionRecord, maxChSize),
	}

	go func() {
		// Track channel worker adds or removes tracks to/from all receivers.
		// TODO(longsleep): Add a way to exit this.
		var logger logrus.FieldLogger
		var index uint64
		for {
			select {
			case trackRecord := <-channel.trackCh:
				remove := trackRecord.remove

				index++
				track := trackRecord.track

				logger = channel.logger.WithFields(logrus.Fields{
					"source":     trackRecord.source.id,
					"sfu_a":      index,
					"remove":     remove,
					"track_ssrc": track.SSRC(),
				})
				logger.Debugln("ooo got local sfu track change")

				channel.connections.IterCb(func(target string, record interface{}) {
					logger.Debugln("ooo sfu selecting", target)
					if target == trackRecord.source.id {
						// Do not publish to self.
						return
					}
					logger = logger.WithField("target", target)

					logger.Debugln("ooo sfu track target")
					targetRecord := record.(*UserRecord)

					var ok bool
					record, ok = targetRecord.connections.Get(trackRecord.source.id)
					if !ok {
						logger.Warnln("ooo updating sfu track to target which does not have a matching source connection")
						return
					}
					connectionRecord := record.(*ConnectionRecord)
					logger.Debugln("ooo sfu using connection", connectionRecord.id)
					connectionRecord.Lock()
					defer connectionRecord.maybeNegotiateAndUnlock()
					pc := connectionRecord.pc
					if pc == nil {
						// No peer connection in this record, skip.
						logger.Debugln("ooo no peer connection on sfu target, skipping")
						return
					}

					if remove {
						logger.WithFields(logrus.Fields{
							"track_id":    track.ID(),
							"track_label": track.Label(),
							"track_kind":  track.Kind(),
							"track_type":  track.PayloadType(),
						}).Debugln("www ooo sfu remove track to target")
						if _, removeErr := connectionRecord.removeTrack(trackRecord); removeErr != nil {
							logger.WithError(removeErr).WithField("track_id", track.ID()).Errorln("www ooo remove sfu track from target failed")
							return
						}
					} else {
						logger.WithFields(logrus.Fields{
							"track_id":    track.ID(),
							"track_label": track.Label(),
							"track_kind":  track.Kind(),
							"track_type":  track.PayloadType(),
						}).Debugln("www ooo sfu add track to target")
						if _, addErr := connectionRecord.addTrack(trackRecord); addErr != nil {
							logger.WithError(addErr).WithField("track_id", track.ID()).Errorln("www ooo add sfu track to target failed")
							return
						}
					}

					if negotiateErr := channel.negotiationNeeded(connectionRecord); negotiateErr != nil {
						logger.WithError(negotiateErr).Errorln("www ooo failed to trigger sfu update track negotiation")
						// TODO(longsleep): Figure out what to do here.
						return
					}
				})
			}
		}
	}()

	go func() {
		// Receiver pc worker adds all existing tracks to newly created peer connections.
		// TODO(longsleep): Add a way to exit this.
		var logger logrus.FieldLogger
		var index uint64
		for {
			select {
			case connectionRecord := <-channel.receiverCh:
				index++

				logger = channel.logger.WithFields(logrus.Fields{
					"wanted": connectionRecord.id,
					"target": connectionRecord.owner.id,
					"pcid":   connectionRecord.pcid,
					"sfu_b":  index,
				})
				logger.Debugln("sss got new peer connection to fill with local sfu track")

				record, found := channel.connections.Get(connectionRecord.id)
				if !found {
					logger.Debugln("sss no connection for wanted")
					break
				}

				logger.Debugln("sss sfu publishing wanted")
				sourceRecord := record.(*UserRecord)

				record, found = sourceRecord.senders.Get("default")
				if !found {
					// Skip source if no sender.
					logger.Debugln("sss skip sfu publishing, no sender")
					break
				}

				func() {
					connectionRecord.Lock()
					defer connectionRecord.maybeNegotiateAndUnlock()
					logger.Debugln("sss sfu using connection", connectionRecord.id)
					pc := connectionRecord.pc
					if pc == nil {
						// No peer connection in our record, do nothing.
						return
					}
					senderRecord := record.(*ConnectionRecord)
					senderRecord.RLock()
					defer senderRecord.RUnlock()
					logger.Debugln("www sss sfu using wanted sender source")

					addedTrack := false
					for id, track := range senderRecord.tracks {
						trackRecord := &TrackRecord{
							track:      track,
							source:     sourceRecord,
							connection: senderRecord,
						}

						// Avoid adding the same track multiple times.
						if _, ok := connectionRecord.tracks[track.SSRC()]; ok {
							continue
						}
						if _, ok := connectionRecord.pending[track.SSRC()]; ok {
							continue
						}

						logger.WithFields(logrus.Fields{
							"sender":          senderRecord.owner.id,
							"sender_track_id": id,
							"track_id":        track.ID(),
							"track_label":     track.Label(),
							"track_kind":      track.Kind(),
							"track_type":      track.PayloadType(),
							"track_ssrc":      track.SSRC(),
						}).Debugln("www sss add sfu track to target")
						if _, addErr := connectionRecord.addTrack(trackRecord); addErr != nil {
							logger.WithError(addErr).WithField("track_ssrc", track.SSRC()).Errorln("www sss add sfu track to target failed")
							return
						} else {
							addedTrack = true
						}
					}
					if addedTrack {
						if negotiateErr := channel.negotiationNeeded(connectionRecord); negotiateErr != nil {
							logger.WithError(negotiateErr).Errorln("www sss failed to trigger sfu add track negotiation")
							// TODO(longsleep): Figure out what to do here.
							return
						}
					} else {
						logger.Debugln("www sss sfu target is already up to date")
					}
				}()

				break
			}
		}
	}()

	return channel, nil
}

func (channel *Channel) handleWebRTCSignalMessage(message *api.RTMTypeWebRTC) error {
	if message.Channel != channel.channel {
		return fmt.Errorf("channel mismatch, got %v, expected %v", message.Channel, channel.channel)
	}

	var record interface{}
	var err error

	//channel.logger.Debugf("xxx signal from: %v", message.Source)
	record = channel.connections.Upsert(message.Source, nil, func(ok bool, userRecord interface{}, n interface{}) interface{} {
		if !ok {
			//channel.logMessage("xxx trigger new userRecord", message)
			userRecordImpl := &UserRecord{
				channel: channel,

				when: time.Now(),
				id:   message.Source,

				connections: cmap.New(),
				senders:     cmap.New(),
			}
			channel.logger.WithField("source", userRecordImpl.id).Debugln("new user")

			// Initiate default sender too.
			defaultSenderConnectionRecord, _ := channel.createSender(userRecordImpl)
			userRecordImpl.senders.Set("default", defaultSenderConnectionRecord)

			userRecord = userRecordImpl
		}
		return userRecord
	})
	sourceRecord := record.(*UserRecord)

	logger := channel.logger.WithFields(logrus.Fields{
		"source": sourceRecord.id,
		"target": message.Target,
	})

	if message.Target == channel.pipeline.Pipeline {
		record = sourceRecord.senders.Upsert("default", nil, func(ok bool, userRecord interface{}, n interface{}) interface{} {
			if !ok {
				defaultSenderConnectionRecord, _ := channel.createSender(sourceRecord)
				userRecord = defaultSenderConnectionRecord
				channel.logger.WithField("source", sourceRecord.id).Debugln("created default sender for pipeline message")
			}
			return userRecord
		})

	} else {
		record = sourceRecord.connections.Upsert(message.Target, nil, func(ok bool, connectionRecord interface{}, n interface{}) interface{} {
			if !ok {
				connectionRecordImpl := NewConnectionRecord(channel.sfu.wsCtx, sourceRecord, message.Target, message.State, nil)
				logger.WithField("initiator", connectionRecordImpl.initiator).Debugln("new connection")

				connectionRecord = connectionRecordImpl
			}
			return connectionRecord
		})
	}
	connectionRecord := record.(*ConnectionRecord)

	// NOTE(longsleep): For now we keep the connectionRecord locked and do everything
	// synchronized with it here. In the future, certain parts below this point
	// might be improved to run outside of this lock.
	connectionRecord.Lock()
	defer connectionRecord.maybeNegotiateAndUnlock()

	if message.Pcid != connectionRecord.rpcid {
		pc := connectionRecord.pc
		if connectionRecord.rpcid == "" {
			if pc != nil && message.Pcid != "" {
				connectionRecord.rpcid = message.Pcid
				logger.WithFields(logrus.Fields{
					"pcid":  connectionRecord.pcid,
					"rpcid": message.Pcid,
				}).Debugln("uuu bound connection to remote")
			}
		} else {
			connectionRecord.rpcid = message.Pcid
			if pc != nil {
				logger.WithFields(logrus.Fields{
					"rpcid_old": connectionRecord.rpcid,
					"rpcid":     message.Pcid,
					"pcid":      connectionRecord.pcid,
				}).Debugln("uuu rpcid has changed, destroying old peer connection")
				connectionRecord.pc = nil
				pc.Close()
			}
		}
	}

	signal := &kwm.RTMDataWebRTCSignal{}
	if err = json.Unmarshal(message.Data, signal); err != nil {
		return fmt.Errorf("failed to parse signal data: %w", err)
	}

	found := false

	if signal.Renegotiate {
		found = true
		if connectionRecord.initiator && (!signal.Noop || connectionRecord.pc == nil) {
			logger.WithField("pcid", connectionRecord.pcid).Debugln("uuu trigger received renegotiate negotiation ", sourceRecord.id)

			if connectionRecord.pc == nil {
				if _, pcErr := channel.createPeerConnection(connectionRecord, sourceRecord, connectionRecord.state); pcErr != nil {
					return fmt.Errorf("uuu failed to create new peer connection: %w", pcErr)
				}
				if connectionRecord.pipeline == nil {
					// Add as receiver connection (sfu sends to it, clients receive) if not a pipeline (where sfu receives).
					channel.receiverCh <- connectionRecord
				}
			}

			if err = channel.negotiationNeeded(connectionRecord); err != nil {
				return fmt.Errorf("uuu failed to trigger negotiation for renegotiate request: %w", err)
			}
		} else {
			// NOTE(longsleep): This should not happen.
			logger.WithField("initiator", connectionRecord.initiator).Warnln("uuu received renegotiate request without being initiator")
			return nil
		}
	}

	if signal.Noop {
		return nil
	}

	if len(signal.Candidate) > 0 {
		found = true
		var candidate webrtc.ICECandidateInit
		if err = json.Unmarshal(signal.Candidate, &candidate); err != nil {
			return fmt.Errorf("failed to parse candidate: %w", err)
		}
		if candidate.Candidate != "" { // Ensure candidate has data, some clients send empty candidates when their ICE has finished.
			if connectionRecord.pc != nil && connectionRecord.pc.RemoteDescription() != nil {
				if err = connectionRecord.pc.AddICECandidate(candidate); err != nil {
					return fmt.Errorf("failed to add ice candidate: %w", err)
				}
			} else {
				connectionRecord.pendingCandidates = append(connectionRecord.pendingCandidates, &candidate)
			}
		}
	}

	if len(signal.SDP) > 0 {
		found = true
		var sdpType webrtc.SDPType
		if err = json.Unmarshal(signal.Type, &sdpType); err != nil {
			return fmt.Errorf("failed to parse sdp signal type: %w", err)
		}
		var sdpString string
		if err = json.Unmarshal(signal.SDP, &sdpString); err != nil {
			return fmt.Errorf("failed to parse sdp payload: %w", err)
		}

		if connectionRecord.pipeline != nil {
			logger.WithField("pcid", connectionRecord.pcid).Debugln("kkk signal for pipeline", sdpType)
		}

		haveRemoteDescription := connectionRecord.pc != nil && connectionRecord.pc.CurrentRemoteDescription() != nil
		if haveRemoteDescription {
			logger.Debugln(">>> kkk sdp signal while already having remote description set")
			timeout := time.After(5 * time.Second)
			for {
				// NOTE(longsleep): This is a workaround for the problem when the remote descriptions is overwritten
				// while the underlaying DTLS transport has not started. If this is the best solution or if it better
				// be detected / avoided somewhere else remains to be seen. For now, this seems to cure the problem.
				wait := false
				for _, sender := range connectionRecord.pc.GetSenders() {
					senderState := sender.Transport().State()
					logger.Debugln(">>> kkk sdp sender transport state", senderState)
					if senderState == webrtc.DTLSTransportStateNew {
						wait = true
						break
					}
				}
				if wait {
					logger.Debugln(">>> kkk sdp sender transport not started yet, wait a bit")
					select {
					case <-timeout:
						// This did now work, kill stuff.
						logger.Debugln(">>> kkk sdp sender transport timeout, resetting")
						connectionRecord.reset(channel.sfu.wsCtx)
						return fmt.Errorf("timeout while waiting for sender dtls transport")
					case <-time.After(100 * time.Millisecond):
						// breaks
					}
				} else {
					break
				}
			}
		}

		sessionDescription := webrtc.SessionDescription{
			Type: sdpType,
			SDP:  sdpString,
		}
		if err = remoteSDPTransform(&sessionDescription); err != nil {
			return fmt.Errorf("failed to transform remote description: %w", err)
		}

		if !connectionRecord.initiator {
			// XXX update webrtc media engine
			channel.logger.Debugln("aaa remote initiator, creating matching webrtc api")
			rtcpfb := []webrtc.RTCPFeedback{
				webrtc.RTCPFeedback{
					Type: webrtc.TypeRTCPFBGoogREMB,
				},
				webrtc.RTCPFeedback{
					Type: webrtc.TypeRTCPFBCCM,
				},
			}
			if experimentUseRTCFBNack {
				rtcpfb = append(rtcpfb, webrtc.RTCPFeedback{
					Type: webrtc.TypeRTCPFBNACK,
				})
				rtcpfb = append(rtcpfb, webrtc.RTCPFeedback{
					Type: "nack pli",
				})
			}
			if experimentUseRTCFBTransportCC {
				rtcpfb = append(rtcpfb, webrtc.RTCPFeedback{
					Type: webrtc.TypeRTCPFBTransportCC,
				})
			}

			// We must use the dynamic media types from the sender in our answers.
			m := webrtc.MediaEngine{}
			if populateErr := m.PopulateFromSDP(sessionDescription); populateErr != nil {
				return fmt.Errorf("aaa failed to populate media engine from remote description: %w", populateErr)
			}
			for _, codec := range m.GetCodecsByKind(webrtc.RTPCodecTypeVideo) {
				channel.logger.Debugln("aaa remote media video codec", codec.PayloadType, codec.Name)
				codec.RTPCodecCapability.RTCPFeedback = rtcpfb
				connectionRecord.rtpPayloadTypes[codec.Name] = codec.PayloadType
			}
			for _, codec := range m.GetCodecsByKind(webrtc.RTPCodecTypeAudio) {
				channel.logger.Debugln("aaa remote media audio codec", codec.PayloadType, codec.Name)
				connectionRecord.rtpPayloadTypes[codec.Name] = codec.PayloadType
			}
			connectionRecord.webrtcMedia = &m
			if connectionRecord.webrtcAPI != nil {
				// NOTE(longsleep): Update media engine is probably not the best of all ideas.
				webrtc.WithMediaEngine(m)(connectionRecord.webrtcAPI)
			}
		}

		ignore := false
		if connectionRecord.pc == nil {
			if connectionRecord.initiator {
				// Received signal without having an connection while being the initator. Ignore incoming data, start new.
				ignore = true
				if err = channel.negotiationNeeded(connectionRecord); err != nil {
					return fmt.Errorf("uuu failed to trigger negotiation for answer signal without peer connection: %w", err)
				}
			}
			if _, pcErr := channel.createPeerConnection(connectionRecord, sourceRecord, connectionRecord.state); pcErr != nil {
				return fmt.Errorf("uuu failed to create new peer connection: %w", pcErr)
			}
			if connectionRecord.pipeline == nil {
				// Add as receiver connection (sfu sends to it, clients receive) if not a pipeline (where sfu receives).
				channel.receiverCh <- connectionRecord
			}
		}
		if !ignore {
			if err = connectionRecord.pc.SetRemoteDescription(sessionDescription); err != nil {
				return fmt.Errorf("failed to set remote description: %w", err)
			}

			for _, candidate := range connectionRecord.pendingCandidates {
				if err = connectionRecord.pc.AddICECandidate(*candidate); err != nil {
					return fmt.Errorf("failed to add queued ice candidate: %w", err)
				}
			}
			connectionRecord.pendingCandidates = nil

			if sdpType == webrtc.SDPTypeOffer {
				// Create answer.
				logger.WithFields(logrus.Fields{
					"pcid":  connectionRecord.pcid,
					"rpcid": connectionRecord.rpcid,
				}).Debugln(">>> kkk offer received from initiator, creating answer")

				// Some tracks might be pending, process now after we have set
				// remote description, in the hope the pending tracks can be
				// added now.
				for ssrc, trackRecord := range connectionRecord.pending {
					logger.WithField("track_ssrc", trackRecord.track.SSRC()).Debugln("ttt adding pending sfu track to target")
					if added, addErr := connectionRecord.addTrack(trackRecord); addErr != nil {
						logger.WithError(addErr).WithField("track_ssrc", trackRecord.track.SSRC()).Errorln("ttt add pending sfu track to target failed")
						delete(connectionRecord.pending, ssrc)
						continue
					} else if !added {
						logger.WithField("track_ssrc", trackRecord.track.SSRC()).Warnln("ttt pending sfu track not added after add")
					} else {
						delete(connectionRecord.pending, ssrc)
					}
					if negotiationErr := channel.negotiationNeeded(connectionRecord); negotiationErr != nil {
						logger.WithError(negotiationErr).Errorln("ttt failed to trigger negotiation after adding pending track")
						// TODO(longsleep): Figure out what to do here.
						break
					}
				}

				if err = channel.createAnswer(connectionRecord, sourceRecord, connectionRecord.state); err != nil {
					return fmt.Errorf("failed to create answer for offer: %w", err)
				}
			}
		}
	}

	if len(signal.TransceiverRequest) > 0 {
		found = true
		channel.logMessage("uuu transceiver request", message)
		if connectionRecord.initiator {
			var transceiverRequest kwm.RTMDataTransceiverRequest
			if err = json.Unmarshal(signal.SDP, &transceiverRequest); err != nil {
				return fmt.Errorf("failed to parse transceiver request payload: %w", err)
			}

			if err = channel.addTransceiver(connectionRecord, sourceRecord, connectionRecord.state, webrtc.NewRTPCodecType(transceiverRequest.Kind), nil); err != nil {
				return fmt.Errorf("failed to add transceivers: %w", err)
			}
		}
	}

	if !found {
		channel.logMessage("xxx unknown webrtc signal", message)
	}

	err = nil // Potentially set by defer.
	return err
}

func (channel *Channel) handleWebRTCHangupMessage(message *api.RTMTypeWebRTC) error {
	if message.Channel != channel.channel {
		return fmt.Errorf("channel mismatch, got %v, expected %v", message.Channel, channel.channel)
	}

	if message.Target != channel.pipeline.Pipeline {
		// Ignore all hangups for non-pipelines.
		return nil
	}

	record, exists := channel.connections.Pop(message.Source)
	if !exists {
		// Ignore non existing.
		return nil
	}
	sourceRecord := record.(*UserRecord)

	sourceRecord.senders.IterCb(func(target string, record interface{}) {
		connectionRecord := record.(*ConnectionRecord)
		connectionRecord.Lock()
		connectionRecord.reset(channel.sfu.wsCtx)
		connectionRecord.Unlock()
	})
	sourceRecord.senders = nil

	sourceRecord.connections.IterCb(func(target string, record interface{}) {
		connectionRecord := record.(*ConnectionRecord)
		connectionRecord.Lock()
		connectionRecord.reset(channel.sfu.wsCtx)
		connectionRecord.Unlock()
	})
	sourceRecord.connections = nil

	return nil
}

func (channel *Channel) logMessage(text string, message interface{}) {
	b, _ := json.MarshalIndent(message, "", "  ")
	channel.logger.Debugln(text, string(b))
}

func (channel *Channel) send(message interface{}) error {
	// TODO(longsleep): Use timemout context.
	// TODO(longsleep): Run asynchronous.
	var writer io.WriteCloser
	writer, err := channel.sfu.ws.Writer(channel.sfu.wsCtx, websocket.MessageText)
	if err != nil {
		return fmt.Errorf("failed to get websocket writer: %w", err)
	}
	defer writer.Close()

	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "\t")
	err = encoder.Encode(message)
	if err != nil {
		return fmt.Errorf("failed to marshal websocket message: %w", err)
	}

	return nil
}

func (channel *Channel) createSender(sourceRecord *UserRecord) (*ConnectionRecord, error) {
	// Initiate default sender too.
	defaultSenderConnectionRecord := NewConnectionRecord(channel.sfu.wsCtx, sourceRecord, channel.pipeline.Pipeline, channel.pipeline.Pipeline, channel.pipeline)

	func() {
		defaultSenderConnectionRecord.Lock()
		defer defaultSenderConnectionRecord.maybeNegotiateAndUnlock()

		// Directly trigger negotiate, for new default sender.
		if negotiateErr := channel.negotiationNeeded(defaultSenderConnectionRecord); negotiateErr != nil {
			channel.logger.WithError(negotiateErr).Errorln("uuu failed to trigger sender negotiation")
			// TODO(longsleep): Figure out what to do here.
		}
	}()

	return defaultSenderConnectionRecord, nil
}

func (channel *Channel) createPeerConnection(connectionRecord *ConnectionRecord, sourceRecord *UserRecord, state string) (*webrtc.PeerConnection, error) {
	if connectionRecord.webrtcAPI == nil {
		if connectionRecord.initiator {
			rtcpfb := []webrtc.RTCPFeedback{
				webrtc.RTCPFeedback{
					Type: webrtc.TypeRTCPFBGoogREMB,
				},
				webrtc.RTCPFeedback{
					Type: webrtc.TypeRTCPFBCCM,
				},
			}
			if experimentUseRTCFBNack {
				rtcpfb = append(rtcpfb, webrtc.RTCPFeedback{
					Type: webrtc.TypeRTCPFBNACK,
				})
				rtcpfb = append(rtcpfb, webrtc.RTCPFeedback{
					Type: "nack pli",
				})
			}
			if experimentUseRTCFBTransportCC {
				rtcpfb = append(rtcpfb, webrtc.RTCPFeedback{
					Type: webrtc.TypeRTCPFBTransportCC,
				})
			}
			m := webrtc.MediaEngine{}
			m.RegisterCodec(webrtc.NewRTPOpusCodec(webrtc.DefaultPayloadTypeOpus, 48000))
			m.RegisterCodec(webrtc.NewRTPVP8CodecExt(webrtc.DefaultPayloadTypeVP8, 90000, rtcpfb))
			for _, codec := range m.GetCodecsByKind(webrtc.RTPCodecTypeVideo) {
				codec.RTPCodecCapability.RTCPFeedback = rtcpfb
				connectionRecord.rtpPayloadTypes[codec.Name] = codec.PayloadType
			}
			for _, codec := range m.GetCodecsByKind(webrtc.RTPCodecTypeAudio) {
				connectionRecord.rtpPayloadTypes[codec.Name] = codec.PayloadType
			}
			connectionRecord.webrtcMedia = &m
			connectionRecord.webrtcAPI = webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(*channel.sfu.webrtcSettings))

		} else {
			if connectionRecord.webrtcMedia == nil {
				return nil, fmt.Errorf("cannot create peer connection without media engine")
			}
			connectionRecord.webrtcAPI = webrtc.NewAPI(webrtc.WithMediaEngine(*connectionRecord.webrtcMedia), webrtc.WithSettingEngine(*channel.sfu.webrtcSettings))
		}
	}

	pc, err := connectionRecord.webrtcAPI.NewPeerConnection(*channel.sfu.webrtcConfiguration)
	if err != nil {
		return nil, fmt.Errorf("error creating peer connection: %w", err)
	}

	iceComplete := connectionRecord.iceComplete

	pcid := utils.NewRandomString(7)
	connectionRecord.pc = pc
	connectionRecord.pcid = pcid
	connectionRecord.isNegotiating = false

	logger := channel.logger.WithFields(logrus.Fields{
		"source": sourceRecord.id,
		"target": connectionRecord.id,
		"pcid":   pcid,
	})

	// Create data channel when initiator.
	if connectionRecord.initiator {
		logger.Debugln("ddd creating data channel")
		if dataChannel, dataChannelErr := pc.CreateDataChannel("kwmbridge-1", nil); dataChannelErr != nil {
			return nil, fmt.Errorf("error creating data channel: %w", dataChannelErr)
		} else {
			dataChannelErr = channel.setupDataChannel(connectionRecord, dataChannel)
			if dataChannelErr != nil {
				return nil, fmt.Errorf("error setting up data channel: %w", dataChannelErr)
			}
		}
	}

	// TODO(longsleep): Bind all event handlers to pcid.
	pc.OnSignalingStateChange(func(signalingState webrtc.SignalingState) {
		logger.Debugln("ppp onSignalingStateChange", signalingState)
		if signalingState == webrtc.SignalingStateStable {
			connectionRecord.Lock()
			if connectionRecord.pc != pc {
				// Replaced, do nothing.
				connectionRecord.Unlock()
				return
			}
			defer connectionRecord.maybeNegotiateAndUnlock()

			if connectionRecord.isNegotiating {
				connectionRecord.isNegotiating = false
				logger.Debugln("nnn negotiation complete")
				if connectionRecord.queuedNegotiation {
					connectionRecord.queuedNegotiation = false
					logger.WithField("pcid", connectionRecord.pcid).Debugln("nnn trigger queued negotiation")
					if negotiationErr := channel.negotiationNeeded(connectionRecord); negotiationErr != nil {
						logger.WithError(negotiationErr).Errorln("nnn failed to trigger queued negotiation")
						// TODO(longsleep): Figure out what to do here.
						return
					}
				}
			}
		}
	})
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		//logger.Debugln("ppp onICECandidate", candidate)
		if candidate == nil {
			// ICE complete.
			logger.Debugln("ppp ICE complete")
			select {
			case <-iceComplete:
				// Huh already complete.
				// NOTE(longsleep): Something is probably wrong, let's hope it will resolve itself.
			default:
				close(iceComplete)
			}
			return
		}

		if experimentICETrickle {
			var candidateInitP *webrtc.ICECandidateInit
			if candidate != nil {
				candidateInit := candidate.ToJSON()
				candidateInitP = &candidateInit
			}

			connectionRecord.Lock()
			defer connectionRecord.Unlock()
			if connectionRecord.pc != pc {
				// Replaced, do nothing.
				return
			}

			if candidateErr := channel.sendCandidate(connectionRecord, sourceRecord, state, candidateInitP); candidateErr != nil {
				logger.WithError(candidateErr).Errorln("ppp failed to send candidate")
				// TODO(longsleep): Figure out what to do here.
			}
		}
	})
	pc.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {
		logger.Debugln("ppp onConnectionStateChange", connectionState, connectionRecord.pipeline)

		if connectionState == webrtc.PeerConnectionStateClosed {
			connectionRecord.Lock()
			defer connectionRecord.Unlock()

			if connectionRecord.pipeline != nil {
				// Having a pipeline connected, means this is a special connection, look further.

				if connectionRecord.pc != nil && connectionRecord.pc != pc {
					// This connection has been replaced already, do nothing.
					logger.Debugln("ppp ignored close on different porentially replaced connection")
					return
				}

				if record, ok := connectionRecord.owner.senders.Get("default"); ok && record == connectionRecord {
					// This is the default record, kill off everything of that user.
					logger.Debugln("ppp default sender is closed, killing off user")
					if record, ok = channel.connections.Pop(connectionRecord.owner.id); ok && record == connectionRecord.owner {
						logger.Debugln("ppp removing killed user from channel")

						connectionRecord.owner.connections.IterCb(func(target string, record interface{}) {
							targetRecord := record.(*ConnectionRecord)
							targetRecord.reset(channel.sfu.wsCtx)
						})
						connectionRecord.owner.connections = nil
						connectionRecord.owner.senders = nil
						connectionRecord.owner.channel = nil
						connectionRecord.owner = nil
					} else {
						logger.Warnln("ppp default sender owner not found in channel")
						return
					}
				}
			}
		}
	})
	pc.OnTrack(func(remoteTrack *webrtc.Track, receiver *webrtc.RTPReceiver) {
		codec := remoteTrack.Codec()
		trackLogger := logger.WithFields(logrus.Fields{
			"track_id":    remoteTrack.ID(),
			"track_label": remoteTrack.Label(),
			"track_kind":  remoteTrack.Kind(),
			"track_type":  remoteTrack.PayloadType(),
			"track_ssrc":  remoteTrack.SSRC(),
			"track_codec": codec.Name,
		})
		trackLogger.Debugln("ttt onTrack")
		connectionRecord.Lock()
		if connectionRecord.pc != pc {
			// Replaced, do nothing.
			connectionRecord.Unlock()
			return
		}
		if connectionRecord.pipeline == nil {
			connectionRecord.Unlock()
			trackLogger.Warnln("ttt received a track but connection is no pipeline, ignoring track")
			return
		}
		if connectionRecord.guid == "" {
			//connectionRecord.guid = newRandomGUID()
			connectionRecord.guid = "stream_" + connectionRecord.owner.id
		}
		if connectionRecord.rtcpCh == nil {
			rtcpCh := make(chan rtcp.Packet, maxChSize)
			connectionRecord.rtcpCh = rtcpCh
			go func(ctx context.Context) {
				for {
					var pkt rtcp.Packet
					select {
					case <-ctx.Done():
						return
					case pkt = <-rtcpCh:
					}

					if pkt == nil {
						// No package, means we have been reset, exit.
						return
					}

					switch pkt.(type) {
					case *rtcp.PictureLossIndication:
						if pc != nil {
							// Request key frame from sender.
							writeErr := pc.WriteRTCP([]rtcp.Packet{pkt})
							if writeErr != nil {
								logger.WithError(writeErr).Errorln("aaa failed to write sfu picture loss indicator")
							}
						}
					case *rtcp.TransportLayerNack:
						// Ignore for now.
					case *rtcp.ReceiverReport:
						// Ignore for now.
					case *rtcp.ReceiverEstimatedMaximumBitrate:
						// Ignore for now.
					case *rtcp.SourceDescription:
						// Ignore for now.
					default:
						logger.Debugln("aaa rtcp message not handled", pkt)
					}
				}
			}(connectionRecord.ctx)
		}

		pubCh := make(chan *rtp.Packet, maxChSize)
		subCh := make(chan *rtp.Packet, maxChSize)

		var localTrack *webrtc.Track
		var localTrackRef uint32
		var isVideo bool

		logger.Debugln("aaa remote track payload type", remoteTrack.PayloadType(), remoteTrack.Codec().Name)
		if codec.Name == webrtc.VP8 {
			// Video.
			if _, ok := connectionRecord.tracks[senderTrackVideo]; ok {
				// XXX(longsleep): What to do here?
				trackLogger.Warnln("ttt received a remote video track but already got one, ignoring track")
				connectionRecord.Unlock()
				return
			}

			trackSSRC := remoteTrack.SSRC()
			//trackSSRC := newRandomUint32()
			trackID := utils.NewRandomGUID()
			//trackID := "vtrack_" + connectionRecord.owner.id
			videoTrack, trackErr := pc.NewTrack(remoteTrack.PayloadType(), trackSSRC, trackID, connectionRecord.guid)
			if trackErr != nil {
				trackLogger.WithError(trackErr).WithField("label", remoteTrack.Label()).Errorln("ttt failed to create new sfu track for video")
				return
			}
			trackLogger.WithField("sfu_track_ssrc", videoTrack.SSRC()).Debugln("ttt created new sfu video track")
			connectionRecord.tracks[senderTrackVideo] = videoTrack
			connectionRecord.Unlock()

			localTrack = videoTrack
			localTrackRef = senderTrackVideo
			isVideo = true

		} else if codec.Name == webrtc.Opus {
			// Audio.
			if _, ok := connectionRecord.tracks[senderTrackAudio]; ok {
				// XXX(longsleep): What to do here?
				trackLogger.Warnln("ttt received a remote audio track but already got one, ignoring track")
				connectionRecord.Unlock()
				return
			}

			trackSSRC := remoteTrack.SSRC()
			//trackSSRC := newRandomUint32()
			trackID := utils.NewRandomGUID()
			//trackID := "atrack_" + connectionRecord.owner.id
			audioTrack, trackErr := pc.NewTrack(remoteTrack.PayloadType(), trackSSRC, trackID, connectionRecord.guid)
			if trackErr != nil {
				trackLogger.WithError(trackErr).WithField("sfu_track_ssr", audioTrack.SSRC()).Errorln("ttt failed to create new sfu track for audio")
				return
			}
			trackLogger.WithField("sfu_track_ssrc", audioTrack.SSRC()).Debugln("ttt created new sfu audio track")
			connectionRecord.tracks[senderTrackAudio] = audioTrack
			connectionRecord.Unlock()

			localTrack = audioTrack
			localTrackRef = senderTrackAudio

		} else {
			logger.Warnln("unsupported remote track codec, track skipped")
			connectionRecord.Unlock()
		}

		if localTrack != nil {
			// Send RTP.
			localCodec := localTrack.Codec()
			go func() {
				var count uint64
				for {
					pkt, ok := <-subCh
					if !ok {
						return
					}

					count++
					// Write to tracks non blocking.
					go func(idx uint64) {
						var payloadType uint8
						channel.connections.IterCb(func(id string, record interface{}) {
							if record == sourceRecord {
								// Skip source.
								return
							}

							targetRecord := record.(*UserRecord)
							record, ok = targetRecord.connections.Get(sourceRecord.id)
							if !ok {
								// No connection for target.
								return
							}
							targetConnection := record.(*ConnectionRecord)

							// Get tracks and payload table.
							targetConnection.RLock()
							track := targetConnection.tracks[pkt.SSRC]
							if payloadType, ok = targetConnection.rtpPayloadTypes[localCodec.Name]; !ok {
								// Found target payload.
								payloadType = localCodec.PayloadType
							}
							targetConnection.RUnlock()
							// Set pkt payload to target type.
							pkt.Header.PayloadType = payloadType

							if track != nil {
								/*if idx%1000 == 0 {
									trackLogger.WithFields(logrus.Fields{
										"count":               idx,
										"target":              targetRecord.id,
										"local_codec":         localCodec.Name,
										"local_payload_type":  localCodec.PayloadType,
										"remote_payload_type": payloadType,
									}).Debugln("ttt send pkg to subscriber")
								}*/
								if writeErr := track.WriteRTP(pkt); writeErr != nil {
									trackLogger.WithError(writeErr).WithField("sfu_track_src", pkt.SSRC).Errorln("ttt failed to write to sfu track")
								}
							} else {
								if idx%1000 == 0 {
									trackLogger.WithField("sfu_track_src", pkt.SSRC).Warnln("ttt unknown target track")
								}
							}
						})
					}(count)
				}
			}()

			// Handle incoming track RTP.
			go func() {
				defer func() {
					close(subCh)
				}()

				for {
					pkt, ok := <-pubCh
					if !ok {
						return
					}

					subCh <- pkt

					if isVideo {
						pushErr := connectionRecord.jitterbuffer.PushRTP(pkt, isVideo)
						if pushErr != nil {
							trackLogger.WithError(pushErr).WithField("sfu_track_src", pkt.SSRC).Errorln("ttt failed to push to jitter")
						}
					}
				}
			}()

			// Read incoming RTP.
			go func() {
				// Make track available for forwarding.
				channel.trackCh <- &TrackRecord{
					track:      localTrack,
					connection: connectionRecord,
					source:     sourceRecord,
				}

				func() {
					defer func() {
						close(pubCh)
					}()

					for {
						pkt, readErr := remoteTrack.ReadRTP()
						if readErr != nil {
							if readErr == io.EOF {
								return
							}
							trackLogger.WithError(readErr).WithField("sfu_track_src", pkt.SSRC).Errorln("ttt failed to read from remote track")
							return
						}

						pubCh <- pkt
					}
				}()
				trackLogger.WithField("sfu_track_src", localTrack.SSRC()).Debugln("ttt sfu track pump exit")

				connectionRecord.Lock()
				delete(connectionRecord.tracks, localTrackRef)
				connectionRecord.Unlock()

				// Make track unavailable for forwarding.
				channel.trackCh <- &TrackRecord{
					track:      localTrack,
					connection: connectionRecord,
					source:     sourceRecord,
					remove:     true,
				}
			}()
		}
	})
	pc.OnDataChannel(func(dataChannel *webrtc.DataChannel) {
		connectionRecord.Lock()
		defer connectionRecord.Unlock()
		if connectionRecord.pc != pc {
			// Replaced, do nothing.
			return
		}

		logger.Debugln("ddd data channel received")
		dataChannelErr := channel.setupDataChannel(connectionRecord, dataChannel)
		if dataChannelErr != nil {
			logger.WithError(dataChannelErr).Errorln("ddd error setting up remote data channel")
		}
	})

	logger.WithFields(logrus.Fields{
		"initiator": connectionRecord.initiator,
	}).Debugln("uuu created new peer connection")

	if experimentAlwaysAddTransceiverToSender && connectionRecord.pipeline != nil {
		channel.logger.WithField("pcid", connectionRecord.pcid).Debugln("kkk adding transceivers to sender")
		transceiverInit := webrtc.RtpTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}
		if _, errTransceiver := pc.AddTransceiver(webrtc.RTPCodecTypeAudio, transceiverInit); errTransceiver != nil {
			channel.logger.WithError(errTransceiver).WithField("pcid", connectionRecord.pcid).Errorln("kkk error adding transceiver for audio")
		}
		if _, errTransceiver := pc.AddTransceiver(webrtc.RTPCodecTypeVideo, transceiverInit); errTransceiver != nil {
			channel.logger.WithError(errTransceiver).WithField("pcid", connectionRecord.pcid).Errorln("kkk error adding transceiver for video")
		}
	}

	if connectionRecord.initiator {
		logger.Debugln("uuu trigger initiator negotiation")
		if err = channel.negotiationNeeded(connectionRecord); err != nil {
			return nil, fmt.Errorf("failed to schedule negotiation: %w", err)
		}
	}

	return pc, nil
}

func (channel *Channel) setupDataChannel(connectionRecord *ConnectionRecord, dataChannel *webrtc.DataChannel) error {
	logger := channel.logger.WithFields(logrus.Fields{
		"pcid":        connectionRecord.pcid,
		"datachannel": dataChannel.Label(),
	})
	logger.Debugln("ddd setting up data channel")

	dataChannel.OnOpen(func() {
		logger.Debugln("ddd data channel open")
	})
	dataChannel.OnClose(func() {
		logger.Debugln("ddd data channel close")

		// NOTE(longsleep): Do the naive approach here, kill the connection when the data channel closed.
		connectionRecord.Lock()
		defer connectionRecord.Unlock()
		connectionRecord.reset(channel.sfu.wsCtx)
	})
	dataChannel.OnError(func(dataChannelErr error) {
		logger.WithError(dataChannelErr).Errorln("ddd data channel error")
	})
	dataChannel.OnMessage(func(message webrtc.DataChannelMessage) {
		if message.IsString {
			logger.WithField("message", string(message.Data)).Debugln("ddd data channel sctp text message")
		} else {
			logger.WithField("size", len(message.Data)).Warnln("ddd data channel sctp binary message, ignored")
		}
	})

	return nil
}

func (channel *Channel) createOffer(connectionRecord *ConnectionRecord, sourceRecord *UserRecord, state string) error {
	sessionDescription, err := connectionRecord.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("failed to create offer: %w", err)
	}
	err = localSDPTransform(&sessionDescription)
	if err != nil {
		return fmt.Errorf("failed to transform local offer description: %w", err)
	}
	err = connectionRecord.pc.SetLocalDescription(sessionDescription)
	if err != nil {
		return fmt.Errorf("failed to set local offer description: %w", err)
	}
	var sdpBytes []byte
	sdpBytes, err = json.MarshalIndent(sessionDescription, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to mashal offer sdp: %w", err)
	}

	out := &api.RTMTypeWebRTC{
		RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
			Type:    api.RTMTypeNameWebRTC,
			Subtype: api.RTMSubtypeNameWebRTCSignal,
		},
		Version: WebRTCPayloadVersion,
		Channel: channel.channel,
		Hash:    channel.hash,
		Target:  sourceRecord.id,
		Source:  connectionRecord.id,
		Group:   channel.group,
		State:   state,
		Pcid:    connectionRecord.pcid,
		Data:    sdpBytes,
	}

	channel.logger.WithField("pcid", connectionRecord.pcid).Debugln(">>> kkk sending offer", sourceRecord.id)
	return channel.send(out)
}

func (channel *Channel) createAnswer(connectionRecord *ConnectionRecord, sourceRecord *UserRecord, state string) error {
	sessionDescription, err := connectionRecord.pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("failed to create answer: %w", err)
	}
	err = localSDPTransform(&sessionDescription)
	if err != nil {
		return fmt.Errorf("failed to transform local answer description: %w", err)
	}
	err = connectionRecord.pc.SetLocalDescription(sessionDescription)
	if err != nil {
		return fmt.Errorf("failed to set local answer description: %w", err)
	}

	var sdpBytes []byte
	sdpBytes, err = json.MarshalIndent(sessionDescription, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to mashal answer sdp: %w", err)
	}

	out := &api.RTMTypeWebRTC{
		RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
			Type:    api.RTMTypeNameWebRTC,
			Subtype: api.RTMSubtypeNameWebRTCSignal,
		},
		Version: WebRTCPayloadVersion,
		Channel: channel.channel,
		Hash:    channel.hash,
		Target:  sourceRecord.id,
		Source:  connectionRecord.id,
		Group:   channel.group,
		State:   state,
		Pcid:    connectionRecord.pcid,
		Data:    sdpBytes,
	}

	func() {
		if !experimentICETrickle {
			<-connectionRecord.iceComplete
		}

		channel.logger.WithField("pcid", connectionRecord.pcid).Debugln(">>> kkk sending answer", sourceRecord.id)
		if sendErr := channel.send(out); sendErr != nil {
			channel.logger.WithError(sendErr).Errorln("failed to send answer description")
			// TODO(longsleep): Figure out what to do here.
		}

		if !connectionRecord.initiator && (connectionRecord.pipeline == nil || !experimentAlwaysAddTransceiverToSender) {
			if transceiversErr := channel.requestMissingTransceivers(connectionRecord, sourceRecord, state); transceiversErr != nil {
				channel.logger.WithError(transceiversErr).Errorln("failed to request missing transceivers")
				// TODO(longsleep): Figure out what to do here.
			}
		}
	}()

	return nil
}

func (channel *Channel) negotiationNeeded(connectionRecord *ConnectionRecord) error {
	select {
	case connectionRecord.needsNegotiation <- true:
	default:
		// channel is full, so already queued.
		channel.logger.WithFields(logrus.Fields{
			"target": connectionRecord.owner.id,
			"source": connectionRecord.id,
			"pcid":   connectionRecord.pcid,
		}).Debugln("nnn negotiation already needed, will only request once")
		return nil
	}
	channel.logger.WithFields(logrus.Fields{
		"target": connectionRecord.owner.id,
		"source": connectionRecord.id,
		"pcid":   connectionRecord.pcid,
	}).Debugln("nnn negotiation needed")
	return nil
}

func (channel *Channel) negotiate(connectionRecord *ConnectionRecord, sourceRecord *UserRecord, state string) error {
	if connectionRecord.initiator {
		if connectionRecord.pc != nil {
			if connectionRecord.isNegotiating {
				connectionRecord.queuedNegotiation = true
				channel.logger.Debugln("nnn initiator already negotiating, queueing")
			} else {
				channel.logger.WithFields(logrus.Fields{
					"target": sourceRecord.id,
					"source": connectionRecord.id,
					"pcid":   connectionRecord.pcid,
				}).Debugln("nnn start negotiation, creating offer")
				if err := channel.createOffer(connectionRecord, sourceRecord, state); err != nil {
					return fmt.Errorf("failed to create offer in negotiate: %w", err)
				}
			}
		}
	} else {
		if connectionRecord.isNegotiating {
			connectionRecord.queuedNegotiation = true
			channel.logger.Debugln("nnn already requested negotiation from initiator, queueing")
		} else {
			channel.logger.WithFields(logrus.Fields{
				"target": sourceRecord.id,
				"source": connectionRecord.id,
				"pcid":   connectionRecord.pcid,
			}).Debugln("nnn requesting negotiation from initiator")
			renegotiate := &kwm.RTMDataWebRTCSignal{
				Renegotiate: true,
			}
			renegotiateBytes, err := json.MarshalIndent(renegotiate, "", "\t")
			if err != nil {
				return fmt.Errorf("nnn failed to mashal renegotiate data: %w", err)
			}
			out := &api.RTMTypeWebRTC{
				RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
					Type:    api.RTMTypeNameWebRTC,
					Subtype: api.RTMSubtypeNameWebRTCSignal,
				},
				Version: WebRTCPayloadVersion,
				Channel: channel.channel,
				Hash:    channel.hash,
				Target:  sourceRecord.id,
				Source:  connectionRecord.id,
				Group:   channel.group,
				State:   state,
				Pcid:    connectionRecord.pcid,
				Data:    renegotiateBytes,
			}
			channel.logger.Debugln(">>> nnn sending renegotiate", sourceRecord.id)
			if err = channel.send(out); err != nil {
				return fmt.Errorf("nnn failed to send renegotiate: %w", err)
			}
		}
	}
	connectionRecord.isNegotiating = true

	return nil
}

func (channel *Channel) requestMissingTransceivers(connectionRecord *ConnectionRecord, sourceRecord *UserRecord, state string) error {
	logger := channel.logger.WithFields(logrus.Fields{
		"target": connectionRecord.id,
		"source": sourceRecord.id,
		"pcid":   connectionRecord.pcid,
	})

	logger.Debugln("kkk requestMissingTransceivers")
	for _, transceiver := range connectionRecord.pc.GetTransceivers() {
		sender := transceiver.Sender()
		logger.Debugln("kkk requestMissingTransceiver has sender", sender != nil)
		if sender == nil {
			continue
		}
		track := sender.Track()
		logger.Debugln("kkk requestMissingTransceiver sender has track", track != nil)
		if track == nil {
			continue
		}
		err := channel.requestMissingTransceiver(connectionRecord, track, transceiver.Direction())
		if err != nil {
			return fmt.Errorf("failed to request missing transceiver: %w", err)
		}
	}

	return nil
}

func (channel *Channel) requestMissingTransceiver(connectionRecord *ConnectionRecord, track *webrtc.Track, direction webrtc.RTPTransceiverDirection) error {
	// Avoid adding the same transceiver multiple times.
	if _, seen := connectionRecord.requestedTransceivers.LoadOrStore(track.SSRC(), nil); seen {
		channel.logger.WithField("track_ssrc", track.SSRC()).Debugln("www kkk requestMissingTransceiver already requested, doing nothing")
		return nil
	}
	channel.logger.Debugln("www kkk requestMissingTransceiver for transceiver", track.Kind(), track.SSRC())
	if err := channel.addTransceiver(connectionRecord, connectionRecord.owner, connectionRecord.state, track.Kind(), &webrtc.RtpTransceiverInit{
		Direction: direction,
	}); err != nil {
		return fmt.Errorf("failed to add missing transceiver: %w", err)
	}
	return nil
}

func (channel *Channel) addTransceiver(connectionRecord *ConnectionRecord, sourceRecord *UserRecord, state string, kind webrtc.RTPCodecType, init *webrtc.RtpTransceiverInit) error {
	if connectionRecord.initiator {
		initArray := []webrtc.RtpTransceiverInit{}
		if init != nil {
			initArray = append(initArray, *init)
		}
		if _, err := connectionRecord.pc.AddTransceiverFromKind(kind, initArray...); err != nil {
			return fmt.Errorf("kkk failed to add transceiver: %w", err)
		}
		channel.logger.WithField("pcid", connectionRecord.pcid).Debugln("kkk trigger negotiation after add transceiver", kind)
		if err := channel.negotiationNeeded(connectionRecord); err != nil {
			return fmt.Errorf("kkk failed to schedule negotiation: %w", err)
		}
	} else {
		if !experimentAddTransceiver {
			channel.logger.Debugln("kkk addTransceiver experiment not enabled")
			return nil
		}
		transceiverRequest := &kwm.RTMDataTransceiverRequest{
			Kind: kind.String(),
		}
		if init != nil {
			// Flip direction.
			var direction webrtc.RTPTransceiverDirection
			switch init.Direction {
			case webrtc.RTPTransceiverDirectionSendonly:
				direction = webrtc.RTPTransceiverDirectionRecvonly
			case webrtc.RTPTransceiverDirectionRecvonly:
				direction = webrtc.RTPTransceiverDirectionSendonly
			case webrtc.RTPTransceiverDirectionSendrecv:
				direction = webrtc.RTPTransceiverDirectionSendrecv
			}
			transceiverRequest.Init = &kwm.RTMDataTransceiverRequestInit{
				Direction: direction.String(),
			}
		}
		channel.logger.WithFields(logrus.Fields{
			"target": sourceRecord.id,
			"source": connectionRecord.id,
			"kind":   transceiverRequest.Kind,
		}).Debugln("www kkk requesting transceivers from initiator")
		transceiverRequestBytes, err := json.MarshalIndent(transceiverRequest, "", "\t")
		if err != nil {
			return fmt.Errorf("kkk failed to mashal transceiver request: %w", err)
		}
		transceiverRequestData := &kwm.RTMDataWebRTCSignal{
			TransceiverRequest: transceiverRequestBytes,
		}
		transceiverRequestDataBytes, err := json.MarshalIndent(transceiverRequestData, "", "\t")
		if err != nil {
			return fmt.Errorf("kkk failed to mashal transceiver request data: %w", err)
		}
		out := &api.RTMTypeWebRTC{
			RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
				Type:    api.RTMTypeNameWebRTC,
				Subtype: api.RTMSubtypeNameWebRTCSignal,
			},
			Version: WebRTCPayloadVersion,
			Channel: channel.channel,
			Hash:    channel.hash,
			Target:  sourceRecord.id,
			Source:  connectionRecord.id,
			Group:   channel.group,
			State:   state,
			Pcid:    connectionRecord.pcid,
			Data:    transceiverRequestDataBytes,
		}
		channel.logger.Debugln(">>> kkk sending transceiver request", sourceRecord.id)
		if err = channel.send(out); err != nil {
			return fmt.Errorf("kkk failed to send transceiver request: %w", err)
		}
	}

	return nil
}

func (channel *Channel) sendCandidate(connectionRecord *ConnectionRecord, sourceRecord *UserRecord, state string, init *webrtc.ICECandidateInit) error {
	candidateBytes, err := json.MarshalIndent(init, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to mashal candidate: %w", err)
	}
	candidateData := &kwm.RTMDataWebRTCSignal{
		Candidate: candidateBytes,
	}
	candidateDataBytes, err := json.MarshalIndent(candidateData, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to mashal candidate data: %w", err)
	}
	out := &api.RTMTypeWebRTC{
		RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
			Type:    api.RTMTypeNameWebRTC,
			Subtype: api.RTMSubtypeNameWebRTCSignal,
		},
		Version: WebRTCPayloadVersion,
		Channel: channel.channel,
		Hash:    channel.hash,
		Target:  sourceRecord.id,
		Source:  connectionRecord.id,
		Group:   channel.group,
		State:   state,
		Pcid:    connectionRecord.pcid,
		Data:    candidateDataBytes,
	}

	//channel.logger.Debugln(">>> sending candidate", sourceRecord.id)
	if err = channel.send(out); err != nil {
		return fmt.Errorf("failed to send candidate: %w", err)
	}

	return nil
}
