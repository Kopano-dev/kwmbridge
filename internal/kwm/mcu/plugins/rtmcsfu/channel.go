/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/orcaman/concurrent-map"
	"github.com/pion/webrtc/v2"
	"github.com/sasha-s/go-deadlock"
	"github.com/sirupsen/logrus"
	"nhooyr.io/websocket"
	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"

	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
)

type Channel struct {
	deadlock.RWMutex

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

	closed chan bool
}

func NewChannel(sfu *RTMChannelSFU, message *api.RTMTypeWebRTC) (*Channel, error) {
	ctx := sfu.wsCtx

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

		closed: make(chan bool, 1),
	}

	go func() {
		// Track channel worker adds or removes tracks to/from all receivers.
		// TODO(longsleep): Add a way to exit this.
		var logger logrus.FieldLogger
		var index uint64
		for {
			select {
			case <-channel.closed:
				return
			case <-ctx.Done():
				return

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

				//channel.connections.IterCb(func(target string, record interface{}) {
				for item := range channel.connections.IterBuffered() {
					func() {
						target := item.Key
						record := item.Val

						logger.Debugln("ooo sfu selecting", target)
						if target == trackRecord.source.id {
							// Do not publish to self.
							return
						}
						targetRecord := record.(*UserRecord)
						if targetRecord.isClosed() {
							// Do not publish to closed.
							return
						}

						logger = logger.WithField("target", target)

						logger.Debugln("ooo sfu track target")

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
						select {
						case <-channel.closed:
							return // Do nothing when closed.
						default:
						}
						pc := connectionRecord.pc
						if pc == nil || connectionRecord.owner == nil {
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

						if negotiateErr := connectionRecord.negotiationNeeded(); negotiateErr != nil {
							logger.WithError(negotiateErr).Errorln("www ooo failed to trigger sfu update track negotiation")
							// TODO(longsleep): Figure out what to do here.
							return
						}
					}()
				}
			}
		}
	}()

	go func() {
		// Receiver pc worker adds all existing tracks to newly created peer connections.
		var index uint64
		for {
			select {
			case <-channel.closed:
				return
			case <-ctx.Done():
				return

			case connectionRecord := <-channel.receiverCh:
				index++

				logger := channel.logger.WithFields(logrus.Fields{
					"wanted": connectionRecord.id,
					"target": connectionRecord.owner.id,
					"pcid":   connectionRecord.pcid,
					"sfu_b":  index,
				})
				logger.Debugln("sss got new peer connection to fill with local sfu tracks")

				record, found := channel.connections.Get(connectionRecord.id)
				if !found {
					logger.Debugln("sss no connection for wanted")
					break
				}

				logger.Debugln("sss sfu publishing wanted")
				sourceRecord := record.(*UserRecord)

				record, found = sourceRecord.publishers.Get("default")
				if !found {
					// Skip source if no sender.
					logger.Debugln("sss skip sfu publishing, no sender")
					break
				}

				func() {
					connectionRecord.Lock()
					defer connectionRecord.maybeNegotiateAndUnlock()
					select {
					case <-channel.closed:
						return // Do nothing when closed.
					default:
					}
					logger.Debugln("sss sfu using connection", connectionRecord.id)
					pc := connectionRecord.pc
					if pc == nil || connectionRecord.owner == nil {
						// No peer connection in our record, do nothing.
						return
					}

					senderRecord := record.(*ConnectionRecord)
					senderRecord.RLock()
					rtcpCh := senderRecord.rtcpCh
					defer senderRecord.RUnlock()
					logger.Debugln("www sss sfu using wanted sender source")

					addedTrack := false
					for id, track := range senderRecord.tracks {
						trackRecord := &TrackRecord{
							track:      track,
							source:     sourceRecord,
							connection: senderRecord,
							rtcpCh:     rtcpCh,
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
						if negotiateErr := connectionRecord.negotiationNeeded(); negotiateErr != nil {
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
	select {
	case <-channel.closed:
		return errors.New("channel is closed")
	default:
	}

	if message.Channel != channel.channel {
		return fmt.Errorf("channel mismatch, got %v, expected %v", message.Channel, channel.channel)
	}

	var record interface{}
	var err error

	//channel.logger.Debugf("xxx signal from: %v", message.Source)
	record = channel.connections.Upsert(message.Source, nil, func(ok bool, userRecord interface{}, n interface{}) interface{} {
		if !ok {
			//channel.logMessage("xxx trigger new userRecord", message)
			userRecordImpl := NewUserRecord(channel, message.Source)
			channel.logger.WithField("source", userRecordImpl.id).Debugln("rrr new user")

			// Initiate default sender too.
			defaultSenderConnectionRecord, _ := channel.createSender(userRecordImpl)
			userRecordImpl.publishers.Set("default", defaultSenderConnectionRecord)

			userRecord = userRecordImpl
		}
		return userRecord
	})
	sourceRecord := record.(*UserRecord)
	if sourceRecord.isClosed() {
		return errors.New("source user is closed")
	}

	logger := channel.logger.WithFields(logrus.Fields{
		"source": sourceRecord.id,
		"target": message.Target,
	})

	if message.Target == channel.pipeline.Pipeline {
		record = sourceRecord.publishers.Upsert("default", nil, func(ok bool, userRecord interface{}, n interface{}) interface{} {
			if !ok {
				defaultSenderConnectionRecord, _ := channel.createSender(sourceRecord)
				userRecord = defaultSenderConnectionRecord
				channel.logger.WithField("source", sourceRecord.id).Debugln("rrr created default sender for pipeline message")
			}
			return userRecord
		})

	} else {
		defer func() {
			if record, ok := channel.connections.Get(message.Target); ok {
				// Target already exists, get connection to source and make sure it works.
				targetRecord := record.(*UserRecord)
				if targetRecord.isClosed() {
					logger.Debugln("rrr target exists, but user is closed")
					return
				}

				if record, ok = targetRecord.connections.Get(message.Source); ok {
					connectionRecord := record.(*ConnectionRecord)
					connectionRecord.Lock()
					defer connectionRecord.maybeNegotiateAndUnlock()
					select {
					case <-channel.closed:
						return // Do nothing when closed.
					default:
					}
					if connectionRecord.pc == nil {
						logger.WithField("initiator", connectionRecord.initiator).Debugln("rrr target exists already, resurrecting connection")
						if connectionRecord.initiator {
							connectionRecord.pcid = ""
							if _, pcErr := connectionRecord.createPeerConnection(""); pcErr != nil {
								logger.WithError(pcErr).Errorln("rrr failed to create peer connection while resurrecting")
							}
							if connectionRecord.pipeline == nil {
								// Add as receiver connection (sfu sends to it, clients receive) if not a pipeline (where sfu receives).
								channel.receiverCh <- connectionRecord
							}
						} else {
							if err := connectionRecord.negotiationNeeded(); err != nil {
								logger.WithError(err).Errorln("rrr failed to trigger negotiation while resurrecting")
							}
						}
					}
				} else {
					logger.Debugln("rrr target exists, but no matching connection")
					return
				}
			}
		}()

		record = sourceRecord.connections.Upsert(message.Target, nil, func(ok bool, connectionRecord interface{}, n interface{}) interface{} {
			if !ok {
				connectionRecordImpl := NewConnectionRecord(channel.sfu.wsCtx, sourceRecord, message.Target, message.State, nil)
				logger.WithField("initiator", connectionRecordImpl.initiator).Debugln("rrr new connection")

				connectionRecord = connectionRecordImpl
			}
			return connectionRecord
		})
	}
	connectionRecord := record.(*ConnectionRecord)
	if connectionRecord.owner.isClosed() {
		return errors.New("target user is closed")
	}

	select {
	case <-channel.closed:
		return nil // Do nothing when closed.
	default:
	}

	// NOTE(longsleep): For now we keep the connectionRecord locked and do everything
	// synchronized with it here. In the future, certain parts below this point
	// might be improved to run outside of this lock.
	connectionRecord.Lock()
	pc := connectionRecord.pc
	needsRelock := false
	unlock := func() {
		if needsRelock {
			panic("unlock already called")
		}
		needsRelock = true
		connectionRecord.Unlock()
	}
	lock := func() error {
		if !needsRelock {
			panic("lock called, but not unlocked")
		}
		needsRelock = false
		connectionRecord.Lock()
		if connectionRecord.pc != pc {
			return errors.New("connection replaced")
		}
		return nil
	}
	defer func() {
		if needsRelock {
			connectionRecord.Lock()
		}
		if connectionRecord.pc == pc {
			connectionRecord.maybeNegotiateAndUnlock()
		} else {
			connectionRecord.Unlock()
		}
	}()
	//defer connectionRecord.maybeNegotiateAndUnlock()

	if message.Pcid != connectionRecord.rpcid {
		if connectionRecord.rpcid == "" {
			if pc != nil && message.Pcid != "" {
				connectionRecord.rpcid = message.Pcid
				logger.WithFields(logrus.Fields{
					"pcid":  connectionRecord.pcid,
					"rpcid": message.Pcid,
				}).Debugln("uuu bound connection to remote")
			}
		} else {
			if pc != nil {
				logger.WithFields(logrus.Fields{
					"rpcid_old": connectionRecord.rpcid,
					"rpcid":     message.Pcid,
					"pcid":      connectionRecord.pcid,
				}).Debugln("uuu rpcid has changed, destroying old peer connection")
				connectionRecord.pc = nil
				connectionRecord.pcid = ""
				pc.Close()
				pc = nil
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
				if newPc, pcErr := connectionRecord.createPeerConnection(message.Pcid); pcErr != nil {
					return fmt.Errorf("uuu failed to create new peer connection: %w", pcErr)
				} else {
					pc = newPc
				}
				if connectionRecord.pipeline == nil {
					// Add as receiver connection (sfu sends to it, clients receive) if not a pipeline (where sfu receives).
					channel.receiverCh <- connectionRecord
				}
			}

			if err = connectionRecord.negotiationNeeded(); err != nil {
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
			if pc != nil && pc.RemoteDescription() != nil {
				if err = pc.AddICECandidate(candidate); err != nil {
					return fmt.Errorf("failed to add ice candidate: %w", err)
				}
			} else {
				connectionRecord.pendingCandidates = append(connectionRecord.pendingCandidates, &candidate)
			}
		}
	}

	if len(signal.SDP) > 0 {
		initiator := connectionRecord.initiator
		unlock()

		found = true
		var sdpType webrtc.SDPType
		if err = json.Unmarshal(signal.Type, &sdpType); err != nil {
			return fmt.Errorf("failed to parse sdp signal type: %w", err)
		}
		var sdpString string
		if err = json.Unmarshal(signal.SDP, &sdpString); err != nil {
			return fmt.Errorf("failed to parse sdp payload: %w", err)
		}

		/*if connectionRecord.pipeline != nil {
			logger.WithField("pcid", connectionRecord.pcid).Debugln("kkk signal for pipeline", sdpType)
		}*/

		haveRemoteDescription := pc != nil && pc.CurrentRemoteDescription() != nil
		if haveRemoteDescription {
			//logger.Debugln(">>> kkk sdp signal while already having remote description set")
			timeout := time.After(5 * time.Second)
			for {
				// NOTE(longsleep): This is a workaround for the problem when the remote descriptions is overwritten
				// while the underlaying DTLS transport has not started. If this is the best solution or if it better
				// be detected / avoided somewhere else remains to be seen. For now, this seems to cure the problem.
				wait := false
				for _, sender := range pc.GetSenders() {
					senderState := sender.Transport().State()
					//logger.Debugln(">>> kkk sdp sender transport state", senderState)
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
						if lockErr := lock(); lockErr != nil {
							logger.WithError(lockErr).Debugln(">>> kkk sdp sender transport timeout, ignored on old connection")
							return nil
						}
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

		if !initiator {
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
				//channel.logger.Debugln("aaa remote media video codec", codec.PayloadType, codec.Name)
				codec.RTPCodecCapability.RTCPFeedback = rtcpfb
				connectionRecord.rtpPayloadTypes[codec.Name] = codec.PayloadType
			}
			if lockErr := lock(); lockErr != nil {
				return nil
			}
			for _, codec := range m.GetCodecsByKind(webrtc.RTPCodecTypeAudio) {
				//channel.logger.Debugln("aaa remote media audio codec", codec.PayloadType, codec.Name)
				connectionRecord.rtpPayloadTypes[codec.Name] = codec.PayloadType
			}
			connectionRecord.webrtcMedia = &m
			if connectionRecord.webrtcAPI != nil {
				// NOTE(longsleep): Update media engine is probably not the best of all ideas.
				webrtc.WithMediaEngine(m)(connectionRecord.webrtcAPI)
			}
			unlock()
		}

		ignore := false
		if pc == nil {
			if initiator {
				// Received signal without having an connection while being the initator. Ignore incoming data, start new.
				ignore = true
				if err = connectionRecord.negotiationNeeded(); err != nil {
					return fmt.Errorf("uuu failed to trigger negotiation for answer signal without peer connection: %w", err)
				}
			}
			if lockErr := lock(); lockErr != nil {
				return nil
			}
			if newPc, pcErr := connectionRecord.createPeerConnection(""); pcErr != nil {
				return fmt.Errorf("uuu failed to create new peer connection: %w", pcErr)
			} else {
				pc = newPc
			}
			if connectionRecord.pipeline == nil {
				// Add as receiver connection (sfu sends to it, clients receive) if not a pipeline (where sfu receives).
				channel.receiverCh <- connectionRecord
			}
			unlock()
		}
		if !ignore {
			if err = pc.SetRemoteDescription(sessionDescription); err != nil {
				return fmt.Errorf("failed to set remote description: %w", err)
			}

			if lockErr := lock(); lockErr != nil {
				return nil
			}

			for _, candidate := range connectionRecord.pendingCandidates {
				if err = pc.AddICECandidate(*candidate); err != nil {
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
					//logger.WithField("track_ssrc", trackRecord.track.SSRC()).Debugln("ttt adding pending sfu track to target")
					if added, addErr := connectionRecord.addTrack(trackRecord); addErr != nil {
						logger.WithError(addErr).WithField("track_ssrc", trackRecord.track.SSRC()).Errorln("ttt add pending sfu track to target failed")
						delete(connectionRecord.pending, ssrc)
						continue
					} else if !added {
						logger.WithField("track_ssrc", trackRecord.track.SSRC()).Warnln("ttt pending sfu track not added after add")
					} else {
						delete(connectionRecord.pending, ssrc)
					}
					if negotiationErr := connectionRecord.negotiationNeeded(); negotiationErr != nil {
						logger.WithError(negotiationErr).Errorln("ttt failed to trigger negotiation after adding pending track")
						// TODO(longsleep): Figure out what to do here.
						break
					}
				}

				if err = connectionRecord.createAnswer(); err != nil {
					return fmt.Errorf("failed to create answer for offer: %w", err)
				}
			}

			unlock()
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

			if err = connectionRecord.addTransceiver(webrtc.NewRTPCodecType(transceiverRequest.Kind), nil); err != nil {
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
	select {
	case <-channel.closed:
		return errors.New("channel is closed")
	default:
	}

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
	if err := sourceRecord.close(); err != nil {
		return err
	}

	return nil
}

func (channel *Channel) logMessage(text string, message interface{}) {
	b, _ := json.MarshalIndent(message, "", "  ")
	channel.logger.Debugln(text, string(b))
}

func (channel *Channel) send(message interface{}) error {
	ctx, cancel := context.WithTimeout(channel.sfu.wsCtx, 30*time.Second)
	defer cancel()

	// TODO(longsleep): Run asynchronous.
	var writer io.WriteCloser
	writer, err := channel.sfu.ws.Writer(ctx, websocket.MessageText)
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
		select {
		case <-channel.closed:
			return // Do nothing when closed.
		default:
		}

		// Directly trigger negotiate, for new default sender.
		if negotiateErr := defaultSenderConnectionRecord.negotiationNeeded(); negotiateErr != nil {
			channel.logger.WithError(negotiateErr).Errorln("uuu failed to trigger sender negotiation")
			// TODO(longsleep): Figure out what to do here.
		}
	}()

	return defaultSenderConnectionRecord, nil
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

	channel.logger.WithField("pcid", connectionRecord.pcid).Debugln(">>> sending candidate", sourceRecord.id)
	if err = channel.send(out); err != nil {
		return fmt.Errorf("failed to send candidate: %w", err)
	}

	return nil
}

func (channel *Channel) Stop() error {
	channel.Lock()
	defer channel.Unlock()

	select {
	case <-channel.closed:
		// Already closed, nothing to do.
		return nil
	default:
	}

	defer close(channel.closed)

	channel.logger.Infoln("stopping channel")

	for item := range channel.connections.IterBuffered() {
		id := item.Key
		record := item.Val
		channel.logger.Debugln("yyy stopping channel user", id)
		userRecord := record.(*UserRecord)
		userRecord.close()
	}

	return nil
}
