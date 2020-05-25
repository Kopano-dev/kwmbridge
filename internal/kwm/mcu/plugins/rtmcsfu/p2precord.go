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
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v2"
	"github.com/sasha-s/go-deadlock"
	"github.com/sirupsen/logrus"
	"stash.kopano.io/kgol/rndm"
	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"

	"stash.kopano.io/kwm/kwmbridge/internal/jitterbuffer"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm/utils"
)

type P2PRecord struct {
	deadlock.RWMutex

	controller *P2PController
	ctx        context.Context
	cancel     context.CancelFunc

	id    string
	token string
	rpcid string

	parent      *ConnectionRecord
	dataChannel *webrtc.DataChannel

	webrtcMedia *webrtc.MediaEngine
	webrtcAPI   *webrtc.API

	pc   *PeerConnection
	pcid string

	pendingCandidates     []*webrtc.ICECandidateInit
	requestedTransceivers *sync.Map

	needsNegotiation  chan bool
	queuedNegotiation bool
	isNegotiating     bool
	iceComplete       chan bool

	guid string

	source    *P2PRecord
	tracks    map[uint32]*webrtc.Track
	senders   map[uint32]*webrtc.RTPSender
	receivers map[uint32]*webrtc.RTPReceiver
	pending   map[uint32]*TrackRecord

	rtcpCh          chan *RTCPRecord
	rtpPayloadTypes map[string]uint8

	jitterBuffer *jitterbuffer.JitterBuffer
}

func NewP2PRecord(parentCtx context.Context, controller *P2PController, parentRecord *ConnectionRecord, dataChannel *webrtc.DataChannel, id string, token string, source *P2PRecord) *P2PRecord {
	ctx, cancel := context.WithCancel(parentCtx)

	record := &P2PRecord{
		controller:  controller,
		parent:      parentRecord,
		dataChannel: dataChannel,
		id:          id,
		token:       token,
		ctx:         ctx,
		cancel:      cancel,

		requestedTransceivers: &sync.Map{},

		needsNegotiation: make(chan bool, 1), // Allow exactly one.
		iceComplete:      make(chan bool),

		source:    source,
		tracks:    make(map[uint32]*webrtc.Track),
		senders:   make(map[uint32]*webrtc.RTPSender),
		receivers: make(map[uint32]*webrtc.RTPReceiver),
		pending:   make(map[uint32]*TrackRecord),

		rtpPayloadTypes: make(map[string]uint8),
	}

	if true {
		// This is a sender (sfu receives), add jitter.
		jitterLogger := controller.logger.WithField("token", token)
		record.jitterBuffer = jitterbuffer.New(token, &jitterbuffer.Config{
			Logger: jitterLogger,

			// TODO(longsleep): Figure out best values.
			PLIInterval:  1,
			RembInterval: 3,
			Bandwidth:    1200, // Starting bitrate.
			MaxBandwidth: 5000,
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
								jitterLogger.WithError(writeErr).Errorln("lll p2p failed to write jitter rtcp")
							}
						}
					}
				}
			}
		}()
	}

	return record
}

func (record *P2PRecord) reset() error {
	record.dataChannel = nil

	pc := record.pc
	if record.pc != nil {
		if !record.parent.initiator {
			record.webrtcMedia = nil
			record.webrtcAPI = nil
		}

		record.pc = nil
		if closeErr := pc.Close(); closeErr != nil {
			record.controller.logger.WithError(closeErr).WithField("pcid", record.pcid).Warnln("error while closing p2p peer connection")
		}
	}

	record.pcid = ""
	record.rpcid = ""
	select {
	case <-record.needsNegotiation:
	default:
	}
	record.queuedNegotiation = false
	record.isNegotiating = false
	record.iceComplete = make(chan bool)
	record.requestedTransceivers = &sync.Map{}
	record.source = nil
	record.tracks = make(map[uint32]*webrtc.Track)
	record.senders = make(map[uint32]*webrtc.RTPSender)
	record.receivers = make(map[uint32]*webrtc.RTPReceiver)
	record.pending = make(map[uint32]*TrackRecord)
	if record.rtcpCh != nil {
		record.rtcpCh <- nil // Closes subscribers.
		record.rtcpCh = nil
	}

	return nil
}

func (record *P2PRecord) clearPeerConnection() {
	pc := record.pc
	if pc == nil {
		return
	}

	pc.Close()

	record.pc = nil
	record.pcid = ""
	record.rpcid = ""
	record.pendingCandidates = nil

	if !record.parent.initiator {
		record.webrtcMedia = nil
		record.webrtcAPI = nil
	}

	select {
	case <-record.needsNegotiation:
	default:
	}
	record.queuedNegotiation = false
	record.isNegotiating = false
	record.iceComplete = make(chan bool)
	record.requestedTransceivers = &sync.Map{}
	record.tracks = make(map[uint32]*webrtc.Track)
	record.pending = make(map[uint32]*TrackRecord)

	if record.rtcpCh != nil {
		record.rtcpCh <- nil
		record.rtcpCh = nil
	}
}

func (p2pRecord *P2PRecord) handleWebRTCSignalMessage(message *api.RTMTypeWebRTC) error {
	//p2pRecord.controller.logger.Debugln("lll p2p webrtc message", message.Subtype)

	controller := p2pRecord.controller

	logger := p2pRecord.controller.logger.WithFields(logrus.Fields{
		"source": message.Source,
	})

	var err error

	// NOTE(longsleep): For now we keep the connectionRecord locked and do everything
	// synchronized with it here. In the future, certain parts below this point
	// might be improved to run outside of this lock.
	p2pRecord.Lock()
	pc := p2pRecord.pc
	needsRelock := false
	unlock := func() {
		if needsRelock {
			panic("unlock already called")
		}
		needsRelock = true
		p2pRecord.Unlock()
	}
	lock := func() error {
		if !needsRelock {
			panic("lock called, but not unlocked")
		}
		needsRelock = false
		p2pRecord.Lock()
		if p2pRecord.pc != pc {
			return errors.New("connection replaced")
		}
		return nil
	}
	defer func() {
		if needsRelock {
			p2pRecord.Lock()
		}
		if p2pRecord.pc == pc {
			p2pRecord.maybeNegotiateAndUnlock()
		} else {
			p2pRecord.Unlock()
		}
	}()

	if message.Pcid != p2pRecord.rpcid {
		if p2pRecord.rpcid == "" {
			if pc != nil && message.Pcid != "" {
				p2pRecord.rpcid = message.Pcid
				logger.WithFields(logrus.Fields{
					"pcid":  p2pRecord.pcid,
					"rpcid": message.Pcid,
				}).Debugln("lll p2p bound connection to remote")
			}
		} else {
			if pc != nil {
				logger.WithFields(logrus.Fields{
					"rpcid_old": p2pRecord.rpcid,
					"rpcid":     message.Pcid,
					"pcid":      p2pRecord.pcid,
				}).Debugln("lll p2p rpcid has changed, destroying old peer connection")
				p2pRecord.clearPeerConnection()
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
		if p2pRecord.parent.initiator && (!signal.Noop || p2pRecord.pc == nil) {
			logger.WithField("pcid", p2pRecord.pcid).Debugln("lll p2p trigger received renegotiate negotiation ", p2pRecord.id)

			if p2pRecord.pc == nil {
				if newPc, pcErr := p2pRecord.createPeerConnection(message.Pcid); pcErr != nil {
					p2pRecord.reset()
					return fmt.Errorf("lll p2p failed to create new peer connection: %w", pcErr)
				} else {
					pc = newPc
				}
			}

			if err = p2pRecord.negotiationNeeded(); err != nil {
				return fmt.Errorf("lll p2p failed to trigger negotiation for renegotiate request: %w", err)
			}
		} else {
			// NOTE(longsleep): This should not happen.
			logger.WithField("initiator", p2pRecord.parent.initiator).Warnln("lll p2p received renegotiate request without being initiator")
			return nil
		}
	}

	if signal.Noop {
		return nil
	}

	if len(signal.Candidate) > 0 {
		//logger.WithField("candidate", string(message.Data)).Debugln(">>> lll p2p inc candiate"))

		found = true
		var candidate webrtc.ICECandidateInit
		if err = json.Unmarshal(signal.Candidate, &candidate); err != nil {
			return fmt.Errorf("failed to parse candidate: %w", err)
		}
		if candidate.Candidate != "" { // Ensure candidate has data, some clients send empty candidates when their ICE has finished.
			if pc != nil && pc.RemoteDescription() != nil {
				if err = pc.AddICECandidate(candidate); err != nil {
					p2pRecord.reset()
					return fmt.Errorf("failed to add ice candidate: %w", err)
				}
			} else {
				p2pRecord.pendingCandidates = append(p2pRecord.pendingCandidates, &candidate)
			}
		}
	}

	if len(signal.SDP) > 0 {
		if experimentLogSDP {
			logger.WithField("sdp", string(message.Data)).Debugln(">>> inc lll p2p sdp")
		}

		initiator := p2pRecord.parent.initiator
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

		sessionDescription := webrtc.SessionDescription{
			Type: sdpType,
			SDP:  sdpString,
		}
		if err = remoteSDPTransform(&sessionDescription); err != nil {
			return fmt.Errorf("failed to transform remote description: %w", err)
		}

		if !initiator {
			// XXX update webrtc media engine
			controller.logger.Debugln("lll p2p remote initiator, creating matching webrtc api")
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
				p2pRecord.reset()
				return fmt.Errorf("aaa failed to populate media engine from remote description: %w", populateErr)
			}
			for _, codec := range m.GetCodecsByKind(webrtc.RTPCodecTypeVideo) {
				//controller.logger.Debugln("jjj p2p remote media video codec", codec.PayloadType, codec.Name)
				codec.RTPCodecCapability.RTCPFeedback = rtcpfb
				p2pRecord.rtpPayloadTypes[codec.Name] = codec.PayloadType
			}
			if lockErr := lock(); lockErr != nil {
				return nil
			}
			for _, codec := range m.GetCodecsByKind(webrtc.RTPCodecTypeAudio) {
				//controller.logger.Debugln("jjj p2p remote media audio codec", codec.PayloadType, codec.Name)
				p2pRecord.rtpPayloadTypes[codec.Name] = codec.PayloadType
			}
			p2pRecord.webrtcMedia = &m
			if p2pRecord.webrtcAPI != nil {
				// NOTE(longsleep): Update media engine is probably not the best of all ideas.
				webrtc.WithMediaEngine(m)(p2pRecord.webrtcAPI)
			}
			unlock()
		}

		ignore := false
		if pc == nil {
			if initiator {
				// Received signal without having an connection while being the initator. Ignore incoming data, start new.
				ignore = true
				if err = p2pRecord.negotiationNeeded(); err != nil {
					return fmt.Errorf("uuu failed to trigger negotiation for answer signal without peer connection: %w", err)
				}
			}
			if lockErr := lock(); lockErr != nil {
				return nil
			}
			if newPc, pcErr := p2pRecord.createPeerConnection(""); pcErr != nil {
				p2pRecord.reset()
				return fmt.Errorf("uuu failed to create new peer connection: %w", pcErr)
			} else {
				pc = newPc
			}

			unlock()
		}
		if !ignore {
			if err = pc.SetRemoteDescription(sessionDescription); err != nil {
				p2pRecord.reset()
				return fmt.Errorf("failed to set remote description: %w", err)
			}

			if lockErr := lock(); lockErr != nil {
				return nil
			}

			for _, candidate := range p2pRecord.pendingCandidates {
				if err = pc.AddICECandidate(*candidate); err != nil {
					p2pRecord.reset()
					return fmt.Errorf("failed to add queued ice candidate: %w", err)
				}
			}
			p2pRecord.pendingCandidates = nil

			if sdpType == webrtc.SDPTypeOffer {
				// Create answer.
				logger.WithFields(logrus.Fields{
					"pcid":  p2pRecord.pcid,
					"rpcid": p2pRecord.rpcid,
				}).Debugln(">>> lll p2p offer received from initiator, creating answer")

				// Some tracks might be pending, process now after we have set
				// remote description, in the hope the pending tracks can be
				// added now.

				for ssrc, trackRecord := range p2pRecord.pending {
					//logger.WithField("track_ssrc", trackRecord.track.SSRC()).Debugln("jjj p2p adding pending sfu track to target")
					if added, addErr := p2pRecord.addTrack(trackRecord); addErr != nil {
						logger.WithError(addErr).WithField("track_ssrc", trackRecord.track.SSRC()).Errorln("lll p2p add pending sfu track to target failed")
						delete(p2pRecord.pending, ssrc)
						continue
					} else if !added {
						logger.WithField("track_ssrc", trackRecord.track.SSRC()).Warnln("ttt pending sfu track not added after add")
						continue
					} else {
						delete(p2pRecord.pending, ssrc)
					}
					if negotiationErr := p2pRecord.negotiationNeeded(); negotiationErr != nil {
						logger.WithError(negotiationErr).Errorln("ttt failed to trigger negotiation after adding pending track")
						// TODO(longsleep): Figure out what to do here.
						break
					}
				}

				if err = p2pRecord.createAnswer(); err != nil {
					p2pRecord.reset()
					return fmt.Errorf("failed to create answer for offer: %w", err)
				}
			}

			unlock()
		}
	}

	if len(signal.TransceiverRequest) > 0 {
		found = true
		controller.logger.Debugln("lll p2p transceiver request", message)
		if p2pRecord.parent.initiator {
			var transceiverRequest kwm.RTMDataTransceiverRequest
			if err = json.Unmarshal(signal.SDP, &transceiverRequest); err != nil {
				return fmt.Errorf("failed to parse transceiver request payload: %w", err)
			}

			if err = p2pRecord.addTransceiver(webrtc.NewRTPCodecType(transceiverRequest.Kind), nil); err != nil {
				return fmt.Errorf("failed to add transceivers: %w", err)
			}
		}
	}

	if !found {
		controller.logger.Debugln("lll p2p xxx unknown webrtc signal", message)
	}

	err = nil // Potentially set by defer.
	return err
}

func (p2pRecord *P2PRecord) negotiationNeeded() error {
	controller := p2pRecord.controller

	select {
	case p2pRecord.needsNegotiation <- true:
	default:
		// channel is full, so already queued.
		return nil
	}
	controller.logger.WithFields(logrus.Fields{
		"source": p2pRecord.token,
		"pcid":   p2pRecord.pcid,
	}).Debugln("lll p2p negotiation needed")
	return nil
}

func (p2pRecord *P2PRecord) createPeerConnection(rpcid string) (*PeerConnection, error) {
	controller := p2pRecord.controller
	channel := p2pRecord.parent.owner.channel

	if p2pRecord.webrtcAPI == nil {
		if p2pRecord.parent.initiator {
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
			m.RegisterCodec(webrtc.NewRTPCodecExt(webrtc.RTPCodecTypeVideo,
				webrtc.VP9,
				90000,
				0,
				"",
				webrtc.DefaultPayloadTypeVP9,
				rtcpfb,
				&codecs.VP9Payloader{},
			))
			for _, codec := range m.GetCodecsByKind(webrtc.RTPCodecTypeVideo) {
				codec.RTPCodecCapability.RTCPFeedback = rtcpfb
				p2pRecord.rtpPayloadTypes[codec.Name] = codec.PayloadType
			}
			for _, codec := range m.GetCodecsByKind(webrtc.RTPCodecTypeAudio) {
				p2pRecord.rtpPayloadTypes[codec.Name] = codec.PayloadType
			}
			p2pRecord.webrtcMedia = &m
			p2pRecord.webrtcAPI = webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(*channel.sfu.webrtcSettings))

		} else {
			if p2pRecord.webrtcMedia == nil {
				return nil, fmt.Errorf("cannot create peer connection without media engine")
			}
			p2pRecord.webrtcAPI = webrtc.NewAPI(webrtc.WithMediaEngine(*p2pRecord.webrtcMedia), webrtc.WithSettingEngine(*channel.sfu.webrtcSettings))
		}
	}

	pc, err := NewPeerConnection(p2pRecord.webrtcAPI, channel.sfu.webrtcConfiguration)
	if err != nil {
		return nil, fmt.Errorf("error creating peer connection: %w", err)
	}

	iceComplete := p2pRecord.iceComplete

	if p2pRecord.pcid == "" {
		//p2pRecord.pcid = utils.NewRandomString(7)
		// TODO(longsleep): Use random string again.
		p2pRecord.pcid = pc.ID()
	}
	p2pRecord.pc = pc
	p2pRecord.rpcid = rpcid
	p2pRecord.isNegotiating = false

	logger := controller.logger.WithFields(logrus.Fields{
		"pcid": p2pRecord.pcid,
	})

	// TODO(longsleep): Add watcher.

	// Create data channel when initiator.
	if p2pRecord.parent.initiator {
		logger.Debugln("jjj p2p creating data channel")
		if _, dataChannelErr := pc.CreateDataChannel(rndm.GenerateRandomString(16), nil); dataChannelErr != nil {
			return nil, fmt.Errorf("error creating data channel: %w", dataChannelErr)
		}
	}

	destroy := func() {
		p2pRecord.Lock()
		logger.Debugln("lll p2p destroy")

		select {
		case <-channel.closed:
			logger.Debugln("lll p2p destroy does nothing, channel is closed")
			p2pRecord.Unlock()
			return // Do nothing when closed.
		default:
		}
		if p2pRecord.pc != nil && p2pRecord.pc != pc {
			logger.Debugln("lll p2p destroy does nothing, pc is already replaced", p2pRecord.pc == nil)
			p2pRecord.Unlock()
			return // Do nothing when replaced.
		}
		defer func() {
			logger.Debugln("lll p2p destroy connection record")
			p2pRecord.reset()
			p2pRecord.Unlock()
		}()
	}

	// TODO(longsleep): Bind all event handlers to pcid.
	pc.OnSignalingStateChange(func(signalingState webrtc.SignalingState) {
		logger.Debugln("lll p2p onSignalingStateChange", signalingState)

		if signalingState == webrtc.SignalingStateStable {
			p2pRecord.Lock()
			select {
			case <-channel.closed:
				p2pRecord.Unlock()
				return // Do nothing when closed.
			default:
			}
			if p2pRecord.pc != pc {
				// Replaced, do nothing.
				p2pRecord.Unlock()
				return
			}
			defer p2pRecord.maybeNegotiateAndUnlock()

			if p2pRecord.isNegotiating {
				p2pRecord.isNegotiating = false
				logger.Debugln("lll p2p negotiation complete")
				if p2pRecord.queuedNegotiation {
					p2pRecord.queuedNegotiation = false
					logger.WithField("pcid", p2pRecord.pcid).Debugln("lll p2p trigger queued negotiation")
					if negotiationErr := p2pRecord.negotiationNeeded(); negotiationErr != nil {
						logger.WithError(negotiationErr).Errorln("lll p2p failed to trigger queued negotiation")
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
			logger.Debugln("lll p2p ICE complete")
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

			p2pRecord.Lock()
			defer p2pRecord.Unlock()
			if p2pRecord.pc != pc {
				// Replaced, do nothing.
				return
			}
			if candidateErr := p2pRecord.sendCandidate(candidateInitP); candidateErr != nil {
				logger.WithError(candidateErr).Errorln("lll p2p failed to send candidate")
				// TODO(longsleep): Figure out what to do here.
			}
		}
	})
	pc.OnICEConnectionStateChange(func(iceConnectionState webrtc.ICEConnectionState) {
		logger.Debugln("lll p2p onICEConnectionStateChange", iceConnectionState)

		if iceConnectionState == webrtc.ICEConnectionStateClosed {
			logger.Debugln("lll p2p ice connection state is closed, trigger destroy")
			destroy()
		}
	})
	pc.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {
		logger.Debugln("lll p2p onConnectionStateChange", connectionState)

		if connectionState == webrtc.PeerConnectionStateClosed || connectionState == webrtc.PeerConnectionStateFailed {
			logger.Debugln("lll p2p connection state is closed, trigger destroy")
			destroy()
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
		trackLogger.Debugln("qqq lll p2p onTrack")
		p2pRecord.Lock()
		select {
		case <-channel.closed:
			p2pRecord.Unlock()
			return // Do nothing when closed.
		default:
		}
		if p2pRecord.pc != pc {
			// Replaced, do nothing.
			p2pRecord.Unlock()
			return
		}
		if p2pRecord.guid == "" {
			p2pRecord.guid = utils.NewRandomGUID()
		}
		if p2pRecord.rtcpCh == nil {
			rtcpCh := make(chan *RTCPRecord, maxChSize)
			p2pRecord.rtcpCh = rtcpCh
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
								logger.WithError(writeErr).Errorln("qqq lll failed to write sfu picture loss indicator")
							}
						}
					case *rtcp.TransportLayerNack:
						logger.Debugln("qqq lll p2p rtcp transport layer nack", pkt)
						nack := pkt.(*rtcp.TransportLayerNack)
						for _, nackPair := range nack.Nacks {
							foundPkt := p2pRecord.jitterBuffer.GetPacket(nack.MediaSSRC, nackPair.PacketID)
							if foundPkt == nil {
								logger.Debugln("qqq lll p2p rtcp transport layer nack not found")
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
										logger.WithError(writeErr).Errorln("qqq lll p2p failed to write rtcp nack indicator to sender")
									}
								}
							} else {
								// We have the missing data, Write pkt again.
								logger.Debugln("qqq lll p2p rtcp transport layer nack write again", rtcpRecord.track != nil)
								if rtcpRecord.track != nil {
									writeErr := rtcpRecord.track.WriteRTP(foundPkt)
									if writeErr != nil {
										logger.WithError(writeErr).Errorln("qqq lll p2p failed to write rtp resend after nack")
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
						logger.Debugln("qqq lll p2p rtcp message not handled", pkt)
					}
				}
			}(p2pRecord.ctx)
		}
		rtcpCh := p2pRecord.rtcpCh

		pubCh := make(chan *rtp.Packet, maxChSize)
		subCh := make(chan *rtp.Packet, maxChSize)

		var localTrack *webrtc.Track
		var localTrackRef uint32
		var isVideo bool

		logger.Debugln("qqq lll p2p remote track payload type", remoteTrack.PayloadType(), remoteTrack.Codec().Name)
		if codec.Name == webrtc.VP8 || codec.Name == webrtc.VP9 {
			// Video.
			if _, ok := p2pRecord.tracks[senderTrackVideo]; ok {
				// XXX(longsleep): What to do here?
				trackLogger.Warnln("qqq lll p2p received a remote video track but already got one, ignoring track")
				p2pRecord.Unlock()
				return
			}

			trackSSRC := remoteTrack.SSRC()
			//trackSSRC := newRandomUint32()
			trackID := utils.NewRandomGUID()
			//trackID := "vtrack_" + connectionRecord.owner.id
			videoTrack, trackErr := pc.NewTrack(remoteTrack.PayloadType(), trackSSRC, trackID, p2pRecord.guid)
			if trackErr != nil {
				trackLogger.WithError(trackErr).WithField("label", remoteTrack.Label()).Errorln("qqq lll p2p failed to create new sfu track for video")
				return
			}
			trackLogger.WithField("sfu_track_ssrc", videoTrack.SSRC()).Debugln("qqq lll p2p created new sfu video track")
			p2pRecord.tracks[senderTrackVideo] = videoTrack
			p2pRecord.receivers[senderTrackVideo] = receiver
			p2pRecord.Unlock()

			localTrack = videoTrack
			localTrackRef = senderTrackVideo
			isVideo = true

		} else {
			trackLogger.Warnln("qqq lll p2p unsupported remote track codec, track skipped")
			p2pRecord.Unlock()
		}

		if localTrack != nil {
			sourceRecord := p2pRecord.parent

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
								record, ok = targetRecord.connections.Get(sourceRecord.owner.id)
								if !ok {
									// No connection for target.
									return
								}

								targetController := record.(*ConnectionRecord).p2p

								record, ok = targetController.callbacks.Get(p2pRecord.token)
								if !ok {
									// No connetion for token.
									return
								}
								targetConnection := record.(*P2PRecord)

								// Lock target, check and get tracks and payload table.
								targetConnection.RLock()
								if targetConnection.parent.owner.isClosed() {
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
									if writeErr := track.WriteRTP(pkt); writeErr != nil {
										trackLogger.WithError(writeErr).WithField("sfu_track_src", pkt.SSRC).Errorln("qqq jjj p2p failed to write to sfu track")
										targetConnection.Lock()
										targetConnection.reset()
										targetConnection.Unlock()
									}
								} else {
									if count%1000 == 0 {
										trackLogger.WithField("sfu_track_src", pkt.SSRC).Warnln("qqq jjj p2p unknown target track")
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
					p2pRecord.jitterBuffer.RemoveBuffer(localTrack.SSRC())
					close(subCh)
				}()

				for {
					pkt, ok := <-pubCh
					if !ok {
						return
					}

					subCh <- pkt

					if isVideo {
						pushErr := p2pRecord.jitterBuffer.PushRTP(pkt, isVideo)
						if pushErr != nil {
							trackLogger.WithError(pushErr).WithField("sfu_track_src", pkt.SSRC).Errorln("qqq lll p2p failed to push to jitter")
						}
					}
				}
			}()

			// Read incoming RTP.
			go func() {
				// Make track available for forwarding.
				channel.trackCh <- &TrackRecord{
					track:      localTrack,
					connection: p2pRecord.parent,
					p2p:        p2pRecord,
					source:     p2pRecord.parent.owner,
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
							trackLogger.WithError(readErr).WithField("sfu_track_src", pkt.SSRC).Errorln("qqq lll p2p failed to read from remote track")
							return
						}

						pubCh <- pkt
					}
				}()
				trackLogger.WithField("sfu_track_src", localTrack.SSRC()).Debugln("qqq lll p2p sfu track pump exit")

				p2pRecord.Lock()
				delete(p2pRecord.tracks, localTrackRef)
				delete(p2pRecord.receivers, localTrackRef)
				p2pRecord.Unlock()

				// Make track unavailable for forwarding.
				channel.trackCh <- &TrackRecord{
					track:      localTrack,
					connection: p2pRecord.parent,
					p2p:        p2pRecord,
					source:     p2pRecord.parent.owner,
					remove:     true,
					rtcpCh:     rtcpCh,
				}
			}()
		}
	})
	/*pc.OnDataChannel(func(dataChannel *webrtc.DataChannel) {
		p2pRecord.Lock()
		defer p2pRecord.Unlock()
	})*/

	logger.WithFields(logrus.Fields{
		"initiator": p2pRecord.parent.initiator,
	}).Debugln("qqq lll p2p created new peer connection")

	if p2pRecord.source != nil {
		err = func() error {
			p2pRecord.source.RLock()
			defer p2pRecord.source.RUnlock()

			for ssrc, track := range p2pRecord.source.tracks {
				logger.WithField("track_ssrc", ssrc).Debugln("qqq lll p2p add source track")
				if _, addErr := p2pRecord.addTrack(&TrackRecord{
					track:      track,
					connection: p2pRecord.source.parent,
					p2p:        p2pRecord.source,
					source:     p2pRecord.parent.owner,
					rtcpCh:     p2pRecord.source.rtcpCh,
				}); addErr != nil {
					return addErr
				}
			}

			return nil
		}()
		if err != nil {
			return nil, fmt.Errorf("failed to add source tracks: %w", err)
		}
	}

	if p2pRecord.parent.initiator {
		//logger.Debugln("qqq lll p2p trigger initiator negotiation")
		if err = p2pRecord.negotiationNeeded(); err != nil {
			return nil, fmt.Errorf("failed to schedule negotiation: %w", err)
		}
	}

	return pc, nil
}

func (record *P2PRecord) maybeNegotiateAndUnlock() {
	defer record.Unlock()

	select {
	case <-record.ctx.Done():
		return
	case needsNegotiation := <-record.needsNegotiation:
		if needsNegotiation {
			if negotiateErr := record.negotiate(false); negotiateErr != nil {
				record.controller.logger.WithError(negotiateErr).Errorln("lll p2p failed to trigger negotiation")
				// TODO(longsleep): Figure out what to do here.
				break
			}
		}
	default:
		// No negotiation required.
	}
}

func (p2pRecord *P2PRecord) createOffer() error {
	controller := p2pRecord.controller

	sessionDescription, err := p2pRecord.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("failed to create offer: %w", err)
	}
	err = localSDPTransform(&sessionDescription)
	if err != nil {
		return fmt.Errorf("failed to transform local offer description: %w", err)
	}
	err = p2pRecord.pc.SetLocalDescription(sessionDescription)
	if err != nil {
		return fmt.Errorf("failed to set local offer description: %w", err)
	}
	var sdpBytes []byte
	sdpBytes, err = json.MarshalIndent(sessionDescription, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to mashal offer sdp: %w", err)
	}

	if experimentLogSDP {
		controller.logger.WithField("sdp", string(sdpBytes)).Debugln("<<< out lll p2p sdp offer")
	}

	out := &api.RTMTypeWebRTC{
		RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
			Type:    api.RTMTypeNameWebRTC,
			Subtype: api.RTMSubtypeNameWebRTCSignal,
		},
		Version: WebRTCPayloadVersion,
		Pcid:    p2pRecord.pcid,
		Source:  p2pRecord.token,
		Data:    sdpBytes,
	}

	controller.logger.WithField("pcid", p2pRecord.pcid).Debugln(">>> lll p2p sending offer", p2pRecord.id)
	return p2pRecord.send(out)
}

func (p2pRecord *P2PRecord) createAnswer() error {
	controller := p2pRecord.controller

	sessionDescription, err := p2pRecord.pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("failed to create answer: %w", err)
	}
	err = localSDPTransform(&sessionDescription)
	if err != nil {
		return fmt.Errorf("failed to transform local answer description: %w", err)
	}
	err = p2pRecord.pc.SetLocalDescription(sessionDescription)
	if err != nil {
		return fmt.Errorf("failed to set local answer description: %w", err)
	}

	var sdpBytes []byte
	sdpBytes, err = json.MarshalIndent(sessionDescription, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to mashal answer sdp: %w", err)
	}

	if experimentLogSDP {
		controller.logger.WithField("sdp", string(sdpBytes)).Debugln("<<< out lll p2p sdp answer")
	}

	out := &api.RTMTypeWebRTC{
		RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
			Type:    api.RTMTypeNameWebRTC,
			Subtype: api.RTMSubtypeNameWebRTCSignal,
		},
		Version: WebRTCPayloadVersion,
		Pcid:    p2pRecord.pcid,
		Source:  p2pRecord.token,
		Data:    sdpBytes,
	}

	func() {
		if !experimentICETrickle {
			<-p2pRecord.iceComplete
		}

		controller.logger.WithField("pcid", p2pRecord.pcid).Debugln(">>> lll p2p sending answer", p2pRecord.id)
		if sendErr := p2pRecord.send(out); sendErr != nil {
			controller.logger.WithError(sendErr).Errorln("lll p2p failed to send answer description")
			// TODO(longsleep): Figure out what to do here.
		}

		if !p2pRecord.parent.initiator {
			if transceiversErr := p2pRecord.requestMissingTransceivers(); transceiversErr != nil {
				controller.logger.WithError(transceiversErr).Errorln("failed to request missing transceivers")
				// TODO(longsleep): Figure out what to do here.
			}
		}
	}()

	return nil
}

func (p2pRecord *P2PRecord) negotiate(noop bool) error {
	controller := p2pRecord.controller
	parentRecord := p2pRecord.parent

	if p2pRecord.parent.initiator {
		if p2pRecord.pc != nil {
			if p2pRecord.isNegotiating {
				p2pRecord.queuedNegotiation = true
				p2pRecord.controller.logger.Debugln("jjj p2p initiator already negotiating, queueing")
			} else {
				p2pRecord.controller.logger.WithFields(logrus.Fields{
					"target": p2pRecord.parent.id,
					"source": p2pRecord.token,
					"pcid":   p2pRecord.pcid,
				}).Debugln("jjj p2p start negotiation, creating offer")
				if err := p2pRecord.createOffer(); err != nil {
					return fmt.Errorf("failed to create offer in negotiate: %w", err)
				}
			}
		}
	} else {
		if p2pRecord.isNegotiating {
			p2pRecord.queuedNegotiation = true
			controller.logger.Debugln("lll p2p already requested negotiation from initiator, queueing")
		} else {
			controller.logger.Debugln("lll p2p requesting negotiation from initiator")
			renegotiate := &kwm.RTMDataWebRTCSignal{
				Renegotiate: true,
				Noop:        noop,
			}
			renegotiateBytes, err := json.MarshalIndent(renegotiate, "", "\t")
			if err != nil {
				return fmt.Errorf("lll p2p failed to mashal renegotiate data: %w", err)
			}
			if p2pRecord.pcid == "" {
				p2pRecord.pcid = utils.NewRandomString(7)
			}
			out := &api.RTMTypeWebRTC{
				RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
					Type:    api.RTMTypeNameWebRTC,
					Subtype: api.RTMSubtypeNameWebRTCSignal,
				},
				Version: WebRTCPayloadVersion,
				Pcid:    p2pRecord.pcid,
				Source:  p2pRecord.token,
				Data:    renegotiateBytes,
			}
			controller.logger.WithField("pcid", p2pRecord.pcid).Debugln(">>> lll p2p sending renegotiate", parentRecord.id)

			if err = p2pRecord.send(out); err != nil {
				return fmt.Errorf("nnn failed to send renegotiate: %w", err)
			}
		}
	}
	p2pRecord.isNegotiating = true

	return nil
}

func (record *P2PRecord) requestMissingTransceivers() error {
	for _, transceiver := range record.pc.GetTransceivers() {
		sender := transceiver.Sender()
		if sender == nil {
			continue
		}
		track := sender.Track()
		if track == nil {
			continue
		}
		err := record.requestMissingTransceiver(track, transceiver.Direction())
		if err != nil {
			return fmt.Errorf("failed to request missing transceiver: %w", err)
		}
	}

	return nil
}

func (record *P2PRecord) requestMissingTransceiver(track *webrtc.Track, direction webrtc.RTPTransceiverDirection) error {
	// Avoid adding the same transceiver multiple times.
	if _, seen := record.requestedTransceivers.LoadOrStore(track.SSRC(), nil); seen {
		//record.controller.logger.WithField("track_ssrc", track.SSRC()).Debugln("jjj p2p requestMissingTransceiver already requested, doing nothing")
		return nil
	}
	//record.controller.logger.Debugln("jjj p2p requestMissingTransceiver for transceiver", track.Kind(), track.SSRC())
	if err := record.addTransceiver(track.Kind(), &webrtc.RtpTransceiverInit{
		Direction: direction,
	}); err != nil {
		return fmt.Errorf("failed to add missing transceiver: %w", err)
	}
	return nil
}

func (record *P2PRecord) addTransceiver(kind webrtc.RTPCodecType, init *webrtc.RtpTransceiverInit) error {
	sourceRecord := record.parent.owner

	if record.parent.initiator {
		initArray := []webrtc.RtpTransceiverInit{}
		if init != nil {
			initArray = append(initArray, *init)
		}
		if _, err := record.pc.AddTransceiverFromKind(kind, initArray...); err != nil {
			record.reset()
			return fmt.Errorf("kkk failed to add transceiver: %w", err)
		}
		record.controller.logger.WithField("pcid", record.pcid).Debugln("jjj p2p trigger negotiation after add transceiver", kind)
		if err := record.negotiationNeeded(); err != nil {
			return fmt.Errorf("jjj p2p failed to schedule negotiation: %w", err)
		}
	} else {
		if !experimentAddTransceiver {
			record.controller.logger.Debugln("jjj p2p2 addTransceiver experiment not enabled")
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
		/*record.controller.logger.WithFields(logrus.Fields{
			"token": record.token,
			"kind":   transceiverRequest.Kind,
		}).Debugln("jjj p2p requesting transceivers from initiator")*/
		transceiverRequestBytes, err := json.MarshalIndent(transceiverRequest, "", "\t")
		if err != nil {
			return fmt.Errorf("jjj p2p failed to mashal transceiver request: %w", err)
		}
		transceiverRequestData := &kwm.RTMDataWebRTCSignal{
			TransceiverRequest: transceiverRequestBytes,
		}
		transceiverRequestDataBytes, err := json.MarshalIndent(transceiverRequestData, "", "\t")
		if err != nil {
			return fmt.Errorf("jjj p2p failed to mashal transceiver request data: %w", err)
		}
		out := &api.RTMTypeWebRTC{
			RTMTypeSubtypeEnvelope: &api.RTMTypeSubtypeEnvelope{
				Type:    api.RTMTypeNameWebRTC,
				Subtype: api.RTMSubtypeNameWebRTCSignal,
			},
			Version: WebRTCPayloadVersion,
			Source:  record.token,
			Pcid:    record.pcid,
			Data:    transceiverRequestDataBytes,
		}
		record.controller.logger.WithField("pcid", record.pcid).Debugln(">>> jjj p2p sending transceiver request", sourceRecord.id)
		if err = record.send(out); err != nil {
			return fmt.Errorf("jjj p2p failed to send transceiver request: %w", err)
		}
	}

	return nil
}

func (record *P2PRecord) addTrack(trackRecord *TrackRecord) (bool, error) {
	senderTrack := trackRecord.track
	senderRecord := trackRecord.connection

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

	if targetCodec != nil && (record.parent.initiator || trackRecord.transceiver) {
		if _, exists := record.tracks[senderTrack.SSRC()]; exists {
			// Track exists, prevent adding the same multiple times;
			return false, fmt.Errorf("track already added")
		}

		// We have the target codec, add track and add to subscriber list.
		track, err := webrtc.NewTrack(targetCodec.PayloadType, senderTrack.SSRC(), senderTrack.ID(), senderTrack.Label(), targetCodec)
		if err != nil {
			return false, fmt.Errorf("failed to create track from sender track: %w", err)
		}

		//record.controller.logger.WithField("track_ssrc", track.SSRC()).Debugln("jjj p2p adding transceiver from track")
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
					record.controller.logger.WithField("track_ssrc", track.SSRC()).Errorln("jjj p2p failed to read rtcp from sender")
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
		if record.parent.initiator {
			return false, fmt.Errorf("unable to create track from sender track: %w", webrtc.ErrCodecNotFound)
		}
		if _, exists := record.pending[senderTrack.SSRC()]; exists {
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

func (record *P2PRecord) removeTrack(trackRecord *TrackRecord) (bool, error) {
	senderTrack := trackRecord.track

	delete(record.pending, senderTrack.SSRC())

	sender, haveSender := record.senders[senderTrack.SSRC()]
	if !haveSender {
		return false, nil
	}

	delete(record.senders, senderTrack.SSRC())
	delete(record.tracks, senderTrack.SSRC())

	if removeErr := record.pc.RemoveTrack(sender); removeErr != nil {
		return true, fmt.Errorf("failed to remove track from record: %w", removeErr)
	}

	return true, nil
}

func (p2pRecord *P2PRecord) sendCandidate(init *webrtc.ICECandidateInit) error {
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
		Source:  p2pRecord.token,
		Pcid:    p2pRecord.pcid,
		Data:    candidateDataBytes,
	}

	p2pRecord.controller.logger.WithField("pcid", p2pRecord.pcid).Debugln(">>> jjj p2p sending candidate", p2pRecord.id)
	if err = p2pRecord.send(out); err != nil {
		return fmt.Errorf("failed to send candidate: %w", err)
	}

	return nil
}

func (p2pRecord *P2PRecord) send(message interface{}) error {
	if p2pRecord.dataChannel == nil {
		return fmt.Errorf("unable to send, have no data channel")
	}

	out, err := json.MarshalIndent(message, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to marshal p2p message: %w", err)
	}

	err = p2pRecord.dataChannel.SendText(string(out))
	if err != nil {
		return fmt.Errorf("failed to write p2p message: %w", err)
	}

	return nil
}
