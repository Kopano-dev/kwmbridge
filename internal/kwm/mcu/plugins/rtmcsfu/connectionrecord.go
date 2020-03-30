/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v2"
	"github.com/sirupsen/logrus"
	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"

	"stash.kopano.io/kwm/kwmbridge/internal/jitterbuffer"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm/utils"
)

type ConnectionRecord struct {
	sync.RWMutex

	owner  *UserRecord
	ctx    context.Context
	cancel context.CancelFunc

	id    string
	rpcid string

	initiator bool
	pipeline  *api.RTMDataWebRTCChannelPipeline

	webrtcMedia *webrtc.MediaEngine
	webrtcAPI   *webrtc.API

	pc    *webrtc.PeerConnection
	pcid  string
	state string

	pendingCandidates     []*webrtc.ICECandidateInit
	requestedTransceivers *sync.Map

	needsNegotiation  chan bool
	queuedNegotiation bool
	isNegotiating     bool
	iceComplete       chan bool

	guid string

	tracks  map[uint32]*webrtc.Track
	senders map[uint32]*webrtc.RTPSender
	pending map[uint32]*TrackRecord

	rtcpCh          chan *RTCPRecord
	rtpPayloadTypes map[string]uint8

	jitterbuffer *jitterbuffer.JitterBuffer
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

		tracks:  make(map[uint32]*webrtc.Track),
		senders: make(map[uint32]*webrtc.RTPSender),
		pending: make(map[uint32]*TrackRecord),

		rtpPayloadTypes: make(map[string]uint8),
	}

	if pipeline != nil {
		// This is a sender (sfu receives), add jitter.
		jitterLogger := owner.channel.logger.WithField("source", owner.id)
		record.jitterbuffer = jitterbuffer.New(target, &jitterbuffer.Config{
			Logger: jitterLogger,

			// TODO(longsleep): Figure out best values.
			PLIInterval:  1,
			RembInterval: 3,
			Bandwidth:    2000,
		})
		err := record.jitterbuffer.Start(ctx)
		if err != nil {
			panic(err)
		}

		// Jitter.
		jitterRtcpCh := record.jitterbuffer.GetRTCPChan()
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

func (record *ConnectionRecord) reset(parentCtx context.Context) {
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
			record.owner.channel.logger.WithField("pcid", record.pcid).Warnln("error while closing peer connection")
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
				}
			}
		}
	}
	record.pcid = ""
	record.pendingCandidates = nil
	close(record.needsNegotiation)
	record.needsNegotiation = make(chan bool, 1)
	record.queuedNegotiation = false
	record.isNegotiating = false
	record.iceComplete = make(chan bool)
	record.requestedTransceivers = &sync.Map{}
	record.tracks = make(map[uint32]*webrtc.Track)
	record.senders = make(map[uint32]*webrtc.RTPSender)
	record.pending = make(map[uint32]*TrackRecord)
	if record.rtcpCh != nil {
		record.rtcpCh <- nil // Closes subscribers.
		record.rtcpCh = nil
	}
}

func (record *ConnectionRecord) maybeNegotiateAndUnlock() {
	defer record.Unlock()

	select {
	case <-record.ctx.Done():
		return
	case needsNegotiation := <-record.needsNegotiation:
		if needsNegotiation {
			record.owner.channel.logger.WithField("pcid", record.pcid).Debugln("<<< nnn needs negotiation", record.owner.id)
			if negotiateErr := record.owner.channel.negotiate(record, record.owner, record.state); negotiateErr != nil {
				record.owner.channel.logger.WithError(negotiateErr).Errorln("nnn failed to trigger negotiation")
				// TODO(longsleep): Figure out what to do here.
				break
			}
		}
	default:
		// No negotiation required.
	}
}

func (record *ConnectionRecord) addTrack(trackRecord *TrackRecord) (bool, error) {
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

		record.owner.channel.logger.WithField("track_ssrc", track.SSRC()).Debugln("aaa adding transceiver from track")
		transceiver, err := record.pc.AddTransceiverFromTrack(track, transceiverInit)
		if err != nil {
			return false, fmt.Errorf("failed to add transceiver from track to record: %w", err)
		}
		sender := transceiver.Sender()
		//}
		record.tracks[track.SSRC()] = track
		record.senders[track.SSRC()] = sender

		senderRecord.RLock()
		rtcpCh := senderRecord.rtcpCh
		senderRecord.RUnlock()
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
			record.owner.channel.logger.WithFields(logrus.Fields{
				"pcid":        record.pcid,
				"rpcid":       record.rpcid,
				"track_codec": senderCodec.Name,
			}).Debugln("ttt track is already pending, do nothing")
			return false, nil
		}

		// Add to pending, and mark transceiver as created.
		trackRecord.transceiver = true
		record.pending[senderTrack.SSRC()] = trackRecord
		// Request missing transceiver.
		if err := record.owner.channel.requestMissingTransceiver(record, senderTrack, transceiverInit.Direction); err != nil {
			return false, fmt.Errorf("failed to request transceiver for sender track: %w", err)
		}

		return false, nil
	}
}

func (record *ConnectionRecord) removeTrack(trackRecord *TrackRecord) (bool, error) {
	senderTrack := trackRecord.track

	delete(record.pending, senderTrack.SSRC())

	sender, haveSender := record.senders[senderTrack.SSRC()]
	if !haveSender {
		record.owner.channel.logger.WithField("track_ssrc", senderTrack.SSRC()).Debugln("ttt tried remove sfu track without sender, nothing to do")
		return false, nil
	}

	delete(record.senders, senderTrack.SSRC())
	delete(record.tracks, senderTrack.SSRC())
	if removeErr := record.pc.RemoveTrack(sender); removeErr != nil {
		return true, fmt.Errorf("failed to remove track from record: %w", removeErr)
	}

	return true, nil
}
