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

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v2"
	"github.com/sasha-s/go-deadlock"
	"github.com/sirupsen/logrus"
	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"

	"stash.kopano.io/kwm/kwmbridge/internal/jitterbuffer"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm/utils"
)

type ConnectionRecord struct {
	deadlock.RWMutex

	owner  *UserRecord
	bound  *UserRecord
	ctx    context.Context
	cancel context.CancelFunc

	onResetHandler func(context.Context) error

	id    string
	rpcid string

	initiator bool
	pipeline  *api.RTMDataWebRTCChannelPipeline

	webrtcMedia *webrtc.MediaEngine
	webrtcAPI   *webrtc.API

	pc    *PeerConnection
	pcid  string
	state string

	pendingCandidates     []*webrtc.ICECandidateInit
	requestedTransceivers *sync.Map

	needsNegotiation  chan bool
	queuedNegotiation bool
	isNegotiating     bool
	iceComplete       chan bool

	guid string

	tracks    map[uint32]*webrtc.Track
	senders   map[uint32]*webrtc.RTPSender
	receivers map[uint32]*webrtc.RTPReceiver
	pending   map[uint32]*TrackRecord

	rtcpCh          chan *RTCPRecord
	rtpPayloadTypes map[string]uint8

	jitterBuffer *jitterbuffer.JitterBuffer

	p2p *P2PController
}

func NewConnectionRecord(parentCtx context.Context, owner *UserRecord, target string, state string, pipeline *api.RTMDataWebRTCChannelPipeline) *ConnectionRecord {
	ctx, cancel := context.WithCancel(parentCtx)

	record := &ConnectionRecord{
		owner:  owner,
		ctx:    ctx,
		cancel: cancel,

		id:        target,
		initiator: utils.ComputeInitiator(owner.id, target),
		pipeline:  pipeline,

		state: state,

		requestedTransceivers: &sync.Map{},

		needsNegotiation: make(chan bool, 1), // Allow exactly one.
		iceComplete:      make(chan bool),

		tracks:    make(map[uint32]*webrtc.Track),
		senders:   make(map[uint32]*webrtc.RTPSender),
		receivers: make(map[uint32]*webrtc.RTPReceiver),
		pending:   make(map[uint32]*TrackRecord),

		rtpPayloadTypes: make(map[string]uint8),
	}

	record.p2p = NewP2PController(record)

	if pipeline != nil {
		// This is a sender (sfu receives), add jitter.
		jitterLogger := owner.channel.logger.WithField("source", owner.id)
		record.jitterBuffer = jitterbuffer.New(target, &jitterbuffer.Config{
			Logger: jitterLogger,

			// TODO(longsleep): Figure out best values.
			PLIInterval:  1,
			RembInterval: 3,
			Bandwidth:    700, // Starting bitrate.
		})
		err := record.jitterBuffer.Start(ctx)
		if err != nil {
			panic(err)
		}

		// Jitter.
		jitterRtcpCh := record.jitterBuffer.GetRTCPChan()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case pkt := <-jitterRtcpCh:
					switch pkt.(type) {
					case *rtcp.TransportLayerNack, *rtcp.ReceiverEstimatedMaximumBitrate, *rtcp.PictureLossIndication:
						record.RLock()
						pc := record.pc
						record.RUnlock()
						if pc != nil {
							writeErr := pc.WriteRTCP([]rtcp.Packet{pkt})
							if writeErr != nil {
								jitterLogger.WithError(writeErr).Errorln("jjj failed to write jitter rtcp")
							}
						}
					}
				}
			}
		}()
	}

	return record
}

func (record *ConnectionRecord) OnReset(handler func(context.Context) error) {
	record.Lock()
	defer record.Unlock()
	record.onResetHandler = handler
}

func (record *ConnectionRecord) reset(parentCtx context.Context) {
	record.p2p.Lock()
	defer record.p2p.Unlock()

	record.cancel()
	record.ctx, record.cancel = context.WithCancel(parentCtx)
	pc := record.pc
	if record.pc != nil {
		if !record.initiator {
			record.webrtcMedia = nil
			record.webrtcAPI = nil
		}

		record.pc = nil
		if closeErr := pc.Close(); closeErr != nil {
			record.owner.channel.logger.WithError(closeErr).WithField("pcid", record.pcid).Warnln("error while closing peer connection")
		}

		// Clean up tracks if this record has a pipeline (means it is sending).
		if record.pipeline != nil {
			for _, track := range record.tracks {
				record.owner.channel.logger.WithField("track_ssrc", track.SSRC()).Debugln("ooo removing sfu track on sender reset")
				record.owner.channel.trackCh <- &TrackRecord{
					track:      track,
					connection: record,
					source:     record.owner,
					remove:     true,
					rtcpCh:     record.rtcpCh,
				}
			}
		}
	}
	record.pcid = ""
	record.rpcid = ""
	record.pendingCandidates = nil
	close(record.needsNegotiation)
	record.needsNegotiation = make(chan bool, 1)
	record.queuedNegotiation = false
	record.isNegotiating = false
	record.iceComplete = make(chan bool)
	record.requestedTransceivers = &sync.Map{}
	record.tracks = make(map[uint32]*webrtc.Track)
	record.senders = make(map[uint32]*webrtc.RTPSender)
	record.receivers = make(map[uint32]*webrtc.RTPReceiver)
	record.pending = make(map[uint32]*TrackRecord)
	if record.rtcpCh != nil {
		record.rtcpCh <- nil // Closes subscribers.
		record.rtcpCh = nil
	}
	record.p2p.reset()
	if record.onResetHandler != nil {
		if resetHandlerErr := record.onResetHandler(parentCtx); resetHandlerErr != nil {
			record.owner.channel.logger.WithError(resetHandlerErr).Warnln("error while resetting connection record")
		}
		record.onResetHandler = nil
	}
	record.bound = nil
}

func (connectionRecord *ConnectionRecord) createPeerConnection(rpcid string) (*PeerConnection, error) {
	sourceRecord := connectionRecord.owner
	if sourceRecord == nil {
		return nil, fmt.Errorf("refusing to create peer connection without owner")
	}

	state := connectionRecord.state
	channel := connectionRecord.owner.channel

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
			m.RegisterCodec(webrtc.NewRTPVP8CodecExt(webrtc.DefaultPayloadTypeVP8, 90000, rtcpfb, ""))
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

	pc, err := NewPeerConnection(connectionRecord.webrtcAPI, channel.sfu.webrtcConfiguration)
	if err != nil {
		return nil, fmt.Errorf("error creating peer connection: %w", err)
	}

	iceComplete := connectionRecord.iceComplete

	if connectionRecord.pcid == "" {
		//connectionRecord.pcid = utils.NewRandomString(7)
		// TODO(longsleep): Use random string again.
		connectionRecord.pcid = pc.ID()
	}
	connectionRecord.pc = pc
	connectionRecord.rpcid = rpcid
	connectionRecord.isNegotiating = false

	logger := channel.logger.WithFields(logrus.Fields{
		"source": sourceRecord.id,
		"target": connectionRecord.id,
		"pcid":   connectionRecord.pcid,
	})

	defer func() {
		// Initialize watcher.
		go func() {
			ctx, cancel := context.WithTimeout(connectionRecord.owner.channel.sfu.wsCtx, 35*time.Second)
			defer cancel()

			for {
				connectionRecord.Lock()
				if connectionRecord.pc != pc {
					connectionRecord.Unlock()
					return
				}
				logger.Debugln("uuu new pc connection check", pc.ConnectionState())
				connectionRecord.Unlock()
				if pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
					// Connected, exiting watcher.
					return
				}

				select {
				case <-time.After(5 * time.Second):
					// breaks
				case <-ctx.Done():
					logger.Debugln("uuu new peer connection timeout waiting for connected state")
					if closeErr := pc.Close(); closeErr != nil {
						logger.WithError(closeErr).Debugln("uuu new peer connection timeout close failed")
					}
					return
				}
			}
		}()
	}()

	// Create data channel when initiator.
	if connectionRecord.initiator {
		logger.Debugln("ddd creating data channel")
		if dataChannel, dataChannelErr := pc.CreateDataChannel("kwmbridge-1", nil); dataChannelErr != nil {
			return nil, fmt.Errorf("error creating data channel: %w", dataChannelErr)
		} else {
			dataChannelErr = connectionRecord.setupDataChannel(pc, dataChannel)
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
			select {
			case <-channel.closed:
				connectionRecord.Unlock()
				return // Do nothing when closed.
			default:
			}
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
					if negotiationErr := connectionRecord.negotiationNeeded(); negotiationErr != nil {
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
			select {
			case <-channel.closed:
				return // Do nothing when closed.
			default:
			}
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
		logger.Debugln("rrr onConnectionStateChange", connectionState)

		if connectionState == webrtc.PeerConnectionStateClosed {
			connectionRecord.Lock()
			defer func() {
				connectionRecord.reset(channel.sfu.wsCtx)
				connectionRecord.Unlock()
			}()
			select {
			case <-channel.closed:
				return // Do nothing when closed.
			default:
			}

			if connectionRecord.pipeline != nil && connectionRecord.owner != nil {
				// Having a pipeline connected, means this is a special connection, look further.

				if connectionRecord.pc != nil && connectionRecord.pc != pc {
					// This connection has been replaced already, do nothing.
					logger.Debugln("rrr ignored close on different porentially replaced connection")
					return
				}

				if record, ok := connectionRecord.owner.publishers.Get("default"); ok && record == connectionRecord {
					// This is the default record, kill off everything of that user.
					logger.Debugln("rrr default sender is closed, killing off user")
					if removed := channel.connections.RemoveCb(connectionRecord.owner.id, func(key string, record interface{}, exists bool) bool {
						if exists && record == connectionRecord.owner {
							return true
						}
						return false
					}); removed {
						logger.Debugln("rrr removed killed user from channel")
						owner := connectionRecord.owner
						connectionRecord.owner = nil
						owner.close()
					} else {
						logger.Debugln("rrr default sender owner of closed peer connection not found in channel")
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
		select {
		case <-channel.closed:
			connectionRecord.Unlock()
			return // Do nothing when closed.
		default:
		}
		if connectionRecord.pc != pc || connectionRecord.owner == nil {
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
			rtcpCh := make(chan *RTCPRecord, maxChSize)
			connectionRecord.rtcpCh = rtcpCh
			go func(ctx context.Context) {
				for {
					var rtcpRecord *RTCPRecord
					select {
					case <-ctx.Done():
						return
					case rtcpRecord = <-rtcpCh:
					}

					if rtcpRecord == nil {
						// No record, means we have been reset, exit.
						return
					}

					pkt := rtcpRecord.packet

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
						//logger.Debugln("aaa rtcp transport layer nack", pkt)
						nack := pkt.(*rtcp.TransportLayerNack)
						for _, nackPair := range nack.Nacks {
							foundPkt := connectionRecord.jitterBuffer.GetPacket(nack.MediaSSRC, nackPair.PacketID)
							if foundPkt == nil {
								//logger.Debugln("aaa rtcp transport layer nack not found")
								// Not found in buffer, notify sender.
								n := &rtcp.TransportLayerNack{
									SenderSSRC: nack.SenderSSRC,
									MediaSSRC:  nack.MediaSSRC,
									Nacks:      []rtcp.NackPair{rtcp.NackPair{PacketID: nackPair.PacketID}},
								}
								if pc != nil {
									// Tell sender that packages were lost.
									writeErr := pc.WriteRTCP([]rtcp.Packet{n})
									if writeErr != nil {
										logger.WithError(writeErr).Errorln("aaa failed to write rtcp nackindicator to sender")
									}
								}
							} else {
								// We have the missing data, Write pkt again.
								//logger.Debugln("aaa rtcp transport layer nack write again", rtcpRecord.track != nil)
								if rtcpRecord.track != nil {
									writeErr := rtcpRecord.track.WriteRTP(foundPkt)
									if writeErr != nil {
										logger.WithError(writeErr).Errorln("aaa failed to write rtp resend after nack")
									}
								}
							}
						}
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
		rtcpCh := connectionRecord.rtcpCh

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
			connectionRecord.receivers[senderTrackVideo] = receiver
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
			connectionRecord.receivers[senderTrackAudio] = receiver
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
					func() {
						var payloadType uint8
						for item := range channel.connections.IterBuffered() {
							func() {
								record := item.Val
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

								// Lock target, check and get tracks and payload table.
								targetConnection.RLock()
								if targetConnection.owner == nil {
									targetConnection.RUnlock()
									return
								}
								track := targetConnection.tracks[pkt.SSRC]
								if payloadType, ok = targetConnection.rtpPayloadTypes[localCodec.Name]; !ok {
									// Found target payload.
									payloadType = localCodec.PayloadType
								}
								targetConnection.RUnlock()
								// Set pkt payload to target type.
								pkt.Header.PayloadType = payloadType

								if track != nil {
									/*if count%1000 == 0 {
										trackLogger.WithFields(logrus.Fields{
											"count":               idx,
											"target":              targetRecord.id,
											"local_codec":         localCodec.Name,
											"local_payload_type":  localCodec.PayloadType,
											"remote_payload_type": payloadType,
										}).Debugln("ttt send pkg to subscriber")
										count = 0
									}*/
									if writeErr := track.WriteRTP(pkt); writeErr != nil {
										trackLogger.WithError(writeErr).WithField("sfu_track_src", pkt.SSRC).Errorln("ttt failed to write to sfu track")
									}
								} else {
									if count%1000 == 0 {
										trackLogger.WithField("sfu_track_src", pkt.SSRC).Warnln("ttt unknown target track")
										count = 0
									}
								}
							}()
						}
					}()
				}
			}()

			// Handle incoming track RTP.
			go func() {
				defer func() {
					connectionRecord.jitterBuffer.RemoveBuffer(localTrack.SSRC())
					close(subCh)
				}()

				for {
					pkt, ok := <-pubCh
					if !ok {
						return
					}

					subCh <- pkt

					if isVideo {
						pushErr := connectionRecord.jitterBuffer.PushRTP(pkt, isVideo)
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
					rtcpCh:     rtcpCh,
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
				delete(connectionRecord.receivers, localTrackRef)
				connectionRecord.Unlock()

				// Make track unavailable for forwarding.
				channel.trackCh <- &TrackRecord{
					track:      localTrack,
					connection: connectionRecord,
					source:     sourceRecord,
					remove:     true,
					rtcpCh:     rtcpCh,
				}
			}()
		}
	})
	pc.OnDataChannel(func(dataChannel *webrtc.DataChannel) {
		connectionRecord.Lock()
		defer connectionRecord.Unlock()
		select {
		case <-channel.closed:
			return // Do nothing when closed.
		default:
		}
		if connectionRecord.pc != pc {
			// Replaced, do nothing.
			return
		}

		//logger.Debugln("ddd data channel received")
		dataChannelErr := connectionRecord.setupDataChannel(pc, dataChannel)
		if dataChannelErr != nil {
			logger.WithError(dataChannelErr).Errorln("ddd error setting up remote data channel")
		}
	})

	logger.WithFields(logrus.Fields{
		"initiator": connectionRecord.initiator,
	}).Debugln("uuu created new peer connection")

	if experimentAlwaysAddTransceiverToSender && connectionRecord.pipeline != nil {
		//channel.logger.WithField("pcid", connectionRecord.pcid).Debugln("kkk adding transceivers to sender")
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
		//logger.Debugln("uuu trigger initiator negotiation")
		if err = connectionRecord.negotiationNeeded(); err != nil {
			return nil, fmt.Errorf("failed to schedule negotiation: %w", err)
		}
	}

	return pc, nil
}

func (record *ConnectionRecord) maybeNegotiateAndUnlock() {
	defer record.Unlock()

	select {
	case <-record.ctx.Done():
		return
	case needsNegotiation := <-record.needsNegotiation:
		if needsNegotiation {
			//record.owner.channel.logger.WithField("pcid", record.pcid).Debugln("<<< nnn needs negotiation", record.owner.id)
			if negotiateErr := record.negotiate(); negotiateErr != nil {
				record.owner.channel.logger.WithError(negotiateErr).Errorln("nnn failed to trigger negotiation")
				// TODO(longsleep): Figure out what to do here.
				break
			}
		}
	default:
		// No negotiation required.
	}
}

func (connectionRecord *ConnectionRecord) negotiationNeeded() error {
	channel := connectionRecord.owner.channel

	select {
	case connectionRecord.needsNegotiation <- true:
	default:
		// channel is full, so already queued.
		/*channel.logger.WithFields(logrus.Fields{
			"target": connectionRecord.owner.id,
			"source": connectionRecord.id,
			"pcid":   connectionRecord.pcid,
		}).Debugln("nnn negotiation already needed, will only request once")*/
		return nil
	}
	channel.logger.WithFields(logrus.Fields{
		"target": connectionRecord.owner.id,
		"source": connectionRecord.id,
		"pcid":   connectionRecord.pcid,
	}).Debugln("nnn negotiation needed")
	return nil
}

func (connectionRecord *ConnectionRecord) createOffer() error {
	sourceRecord := connectionRecord.owner
	state := connectionRecord.state
	channel := connectionRecord.owner.channel

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

func (connectionRecord *ConnectionRecord) createAnswer() error {
	sourceRecord := connectionRecord.owner
	state := connectionRecord.state
	channel := connectionRecord.owner.channel

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
			if transceiversErr := connectionRecord.requestMissingTransceivers(); transceiversErr != nil {
				channel.logger.WithError(transceiversErr).Errorln("failed to request missing transceivers")
				// TODO(longsleep): Figure out what to do here.
			}
		}
	}()

	return nil
}

func (connectionRecord *ConnectionRecord) negotiate() error {
	sourceRecord := connectionRecord.owner
	state := connectionRecord.state
	channel := connectionRecord.owner.channel

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
				if err := connectionRecord.createOffer(); err != nil {
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
			if connectionRecord.pcid == "" {
				connectionRecord.pcid = utils.NewRandomString(7)
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
			channel.logger.WithField("pcid", connectionRecord.pcid).Debugln(">>> nnn sending renegotiate", sourceRecord.id)
			if err = channel.send(out); err != nil {
				return fmt.Errorf("nnn failed to send renegotiate: %w", err)
			}
		}
	}
	connectionRecord.isNegotiating = true

	return nil
}

func (connectionRecord *ConnectionRecord) requestMissingTransceivers() error {
	/*logger := channel.logger.WithFields(logrus.Fields{
		"target": connectionRecord.id,
		"source": sourceRecord.id,
		"pcid":   connectionRecord.pcid,
	})*/

	//logger.Debugln("kkk requestMissingTransceivers")
	for _, transceiver := range connectionRecord.pc.GetTransceivers() {
		sender := transceiver.Sender()
		//logger.Debugln("kkk requestMissingTransceiver has sender", sender != nil)
		if sender == nil {
			continue
		}
		track := sender.Track()
		//logger.Debugln("kkk requestMissingTransceiver sender has track", track != nil)
		if track == nil {
			continue
		}
		err := connectionRecord.requestMissingTransceiver(track, transceiver.Direction())
		if err != nil {
			return fmt.Errorf("failed to request missing transceiver: %w", err)
		}
	}

	return nil
}

func (connectionRecord *ConnectionRecord) requestMissingTransceiver(track *webrtc.Track, direction webrtc.RTPTransceiverDirection) error {
	// Avoid adding the same transceiver multiple times.
	if _, seen := connectionRecord.requestedTransceivers.LoadOrStore(track.SSRC(), nil); seen {
		//channel.logger.WithField("track_ssrc", track.SSRC()).Debugln("www kkk requestMissingTransceiver already requested, doing nothing")
		return nil
	}
	//channel.logger.Debugln("www kkk requestMissingTransceiver for transceiver", track.Kind(), track.SSRC())
	if err := connectionRecord.addTransceiver(track.Kind(), &webrtc.RtpTransceiverInit{
		Direction: direction,
	}); err != nil {
		return fmt.Errorf("failed to add missing transceiver: %w", err)
	}
	return nil
}

func (connectionRecord *ConnectionRecord) addTransceiver(kind webrtc.RTPCodecType, init *webrtc.RtpTransceiverInit) error {
	sourceRecord := connectionRecord.owner
	state := connectionRecord.state
	channel := connectionRecord.owner.channel

	if connectionRecord.initiator {
		initArray := []webrtc.RtpTransceiverInit{}
		if init != nil {
			initArray = append(initArray, *init)
		}
		if _, err := connectionRecord.pc.AddTransceiverFromKind(kind, initArray...); err != nil {
			connectionRecord.reset(channel.sfu.wsCtx)
			return fmt.Errorf("kkk failed to add transceiver: %w", err)
		}
		channel.logger.WithField("pcid", connectionRecord.pcid).Debugln("kkk trigger negotiation after add transceiver", kind)
		if err := connectionRecord.negotiationNeeded(); err != nil {
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
		/*channel.logger.WithFields(logrus.Fields{
			"target": sourceRecord.id,
			"source": connectionRecord.id,
			"kind":   transceiverRequest.Kind,
		}).Debugln("www kkk requesting transceivers from initiator")*/
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
		channel.logger.WithField("pcid", connectionRecord.pcid).Debugln(">>> kkk sending transceiver request", sourceRecord.id)
		if err = channel.send(out); err != nil {
			return fmt.Errorf("kkk failed to send transceiver request: %w", err)
		}
	}

	return nil
}

func (record *ConnectionRecord) addTrack(trackRecord *TrackRecord) (bool, error) {
	sourceRecord := trackRecord.source
	senderTrack := trackRecord.track
	senderRecord := trackRecord.connection

	switch {
	case record.bound == nil:
		// Bind record to sender source.
		record.bound = sourceRecord
	case record.bound != sourceRecord:
		return false, fmt.Errorf("track sender mismatch in add track")
	}

	var targetCodec *webrtc.RTPCodec
	senderCodec := senderTrack.Codec()
	for _, codec := range record.webrtcMedia.GetCodecsByKind(senderTrack.Kind()) {
		if codec.Name == senderCodec.Name {
			targetCodec = codec
			break
		}
	}

	transceiverInit := webrtc.RtpTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	}

	if targetCodec != nil && (record.initiator || trackRecord.transceiver) {
		if _, exists := record.tracks[senderTrack.SSRC()]; exists {
			// Track exists, prevent adding the same multiple times;
			return false, fmt.Errorf("track already added")
		}

		// We have the target codec, add track and add to subscriber list.
		track, err := webrtc.NewTrack(targetCodec.PayloadType, senderTrack.SSRC(), senderTrack.ID(), senderTrack.Label(), targetCodec)
		if err != nil {
			return false, fmt.Errorf("failed to create track from sender track: %w", err)
		}

		//record.owner.channel.logger.WithField("track_ssrc", track.SSRC()).Debugln("aaa adding transceiver from track")
		var sender *webrtc.RTPSender
		if false {
			// Pion before 1ba672fd111a needs manual add of transceiver. Later
			// revisions do this automatically. Since the new behavior is how
			// it should be, this section can eventually go away. See
			// https://github.com/pion/webrtc/issues/1171 for details.
			transceiver, addErr := record.pc.AddTransceiverFromTrack(track, transceiverInit)
			if addErr != nil {
				return false, fmt.Errorf("failed to add transceiver from track to record: %w", addErr)
			}
			sender = transceiver.Sender()
		} else {
			sender, err = record.pc.AddTrack(track)
			if err != nil {
				return false, fmt.Errorf("failed to add track to record: %w", err)
			}
		}

		record.tracks[track.SSRC()] = track
		record.senders[track.SSRC()] = sender

		rtcpCh := trackRecord.rtcpCh
		go func() {
			for {
				pkts, err := sender.ReadRTCP()
				if err != nil {
					if err == io.EOF {
						return
					}
					record.owner.channel.logger.WithField("track_ssrc", track.SSRC()).Errorln("aaa failed to read rtcp from sender")
				}
				senderRecord.RLock()
				if rtcpCh != senderRecord.rtcpCh {
					// Channel has changed, this usually means the sender record has been reset. Stop anything we do here.
					senderRecord.RUnlock()
					return
				}
				senderRecord.RUnlock()
				for _, pkt := range pkts {
					if pkt != nil {
						rtcpCh <- &RTCPRecord{
							packet: pkt,
							track:  track,
						}
					}
				}
			}
		}()

		return true, nil
	} else {
		if record.initiator {
			return false, fmt.Errorf("unable to create track from sender track: %w", webrtc.ErrCodecNotFound)
		}
		if _, exists := record.pending[senderTrack.SSRC()]; exists {
			// Track already pending, do nothing.
			/*record.owner.channel.logger.WithFields(logrus.Fields{
				"pcid":        record.pcid,
				"rpcid":       record.rpcid,
				"track_codec": senderCodec.Name,
			}).Debugln("ttt track is already pending, do nothing")*/
			return false, nil
		}

		// Add to pending, and mark transceiver as created.
		trackRecord.transceiver = true
		record.pending[senderTrack.SSRC()] = trackRecord
		// Request missing transceiver.
		if err := record.requestMissingTransceiver(senderTrack, transceiverInit.Direction); err != nil {
			return false, fmt.Errorf("failed to request transceiver for sender track: %w", err)
		}

		return false, nil
	}
}

func (record *ConnectionRecord) removeTrack(trackRecord *TrackRecord) (bool, error) {
	sourceRecord := trackRecord.source
	senderTrack := trackRecord.track

	switch {
	case record.bound == nil:
		// Ignore track removal requests when not bound.
		return false, nil
	case record.bound != sourceRecord:
		return false, fmt.Errorf("track sender mismatch in remove track")
	}

	delete(record.pending, senderTrack.SSRC())

	sender, haveSender := record.senders[senderTrack.SSRC()]
	if !haveSender {
		//record.owner.channel.logger.WithField("track_ssrc", senderTrack.SSRC()).Debugln("ttt tried remove sfu track without sender, nothing to do")
		return false, nil
	}

	delete(record.senders, senderTrack.SSRC())
	delete(record.tracks, senderTrack.SSRC())

	if removeErr := record.pc.RemoveTrack(sender); removeErr != nil {
		return true, fmt.Errorf("failed to remove track from record: %w", removeErr)
	}

	return true, nil
}

func (connectionRecord *ConnectionRecord) setupDataChannel(pc *PeerConnection, dataChannel *webrtc.DataChannel) error {
	channel := connectionRecord.owner.channel

	logger := channel.logger.WithFields(logrus.Fields{
		"pcid":        connectionRecord.pcid,
		"datachannel": dataChannel.Label(),
	})
	//logger.Debugln("ddd setting up data channel")

	dataChannel.OnOpen(func() {
		//logger.Debugln("ddd data channel open")
	})
	dataChannel.OnClose(func() {
		logger.Debugln("ddd data channel close")

		// NOTE(longsleep): Do the naive approach here, kill the connection when the data channel closed.
		connectionRecord.Lock()
		defer connectionRecord.Unlock()
		if connectionRecord.pc == pc {
			connectionRecord.reset(channel.sfu.wsCtx)
			connectionRecord.p2p.handleClose(pc, dataChannel)
		}
	})
	dataChannel.OnError(func(dataChannelErr error) {
		logger.WithError(dataChannelErr).Errorln("ddd data channel error")
	})
	dataChannel.OnMessage(func(raw webrtc.DataChannelMessage) {
		if raw.IsString {
			logger.WithField("message", string(raw.Data)).Debugln("ddd data channel sctp text message")
			connectionRecord.p2p.handleData(pc, dataChannel, raw)
		} else {
			logger.WithField("size", len(raw.Data)).Warnln("ddd data channel sctp binary message, ignored")
		}
	})

	return nil
}
