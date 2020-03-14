/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package kwmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/orcaman/concurrent-map"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v2"
	"github.com/sirupsen/logrus"
	"nhooyr.io/websocket"
	"stash.kopano.io/kgol/rndm"
	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"

	"stash.kopano.io/kwm/kwmbridge/internal/bpool"
)

// Experiments for inccomplete features.
const (
	experimentAddTransceiver                    = true
	experimentAlwaysAddTransceiverWhenInitiator = false
	experimentAlwaysAddTransceiverToSender      = true
	experimentICETrickle                        = false
)

/**
* WebRTC payload version. All WebRTC payloads will include this value and
* clients can use it to check if they are compatible with the received
* data. This client will reject all messages which are from received with
* older version than defined here.
 */
const WebRTCPayloadVersion = 20180703

/*
var (
	globalAudioTransceiver = &webrtc.RtpTransceiverInit{}
	globalVideoTransceiver = &webrtc.RtpTransceiverInit{}
)*/

type RTMChannelSFU struct {
	sync.RWMutex

	options *MCUOptions
	logger  logrus.FieldLogger

	wsCtx    context.Context
	wsCancel context.CancelFunc
	ws       *websocket.Conn

	channel *RTMChannelSFUChannel

	webrtcAPI           *webrtc.API
	webrtcConfiguration *webrtc.Configuration
}

func NewRTMChannelSFU(attach *WebsocketMessage, ws *websocket.Conn, options *MCUOptions) (*RTMChannelSFU, error) {
	logger := options.Logger.WithFields(logrus.Fields{
		"type":        "RTMChannelSFU",
		"transaction": attach.Transaction,
		"plugin":      attach.Plugin,
	})

	logger.Infoln("starting rtm channel sfu")

	m := webrtc.MediaEngine{}
	m.RegisterCodec(webrtc.NewRTPVP8Codec(webrtc.DefaultPayloadTypeVP8, 90000))
	m.RegisterCodec(webrtc.NewRTPOpusCodec(webrtc.DefaultPayloadTypeOpus, 48000))

	s := webrtc.SettingEngine{
		LoggerFactory: &loggerFactory{logger},
	}
	s.SetTrickle(experimentICETrickle)
	s.SetLite(true)

	if len(options.Config.ICEInterfaces) > 0 {
		logger.WithField("interfaces", options.Config.ICEInterfaces).Debugln("enabling ICE interface filter")
		iceInterfaceFilterMap := make(map[string]bool)
		for _, ifName := range options.Config.ICEInterfaces {
			iceInterfaceFilterMap[ifName] = true
		}
		s.SetInterfaceFilter(func(i string) bool {
			return iceInterfaceFilterMap[i]
		})
	}

	if len(options.Config.ICENetworkTypes) > 0 {
		candidateTypes := make([]webrtc.NetworkType, 0)
		for _, networkTypeString := range options.Config.ICENetworkTypes {
			var nt webrtc.NetworkType
			switch strings.ToLower(networkTypeString) {
			case "udp4":
				nt = webrtc.NetworkTypeUDP4
			case "udp6":
				nt = webrtc.NetworkTypeUDP6
			case "tcp4":
				nt = webrtc.NetworkTypeTCP4
			case "tcp6":
				nt = webrtc.NetworkTypeTCP6
			default:
				logger.WithField("type", networkTypeString).Warnln("unsupported network type, skipped")
				continue
			}
			candidateTypes = append(candidateTypes, nt)
		}
		if len(candidateTypes) == 0 {
			logger.Errorln("ICE candidate network type list is empty, continuing anyway")
		}
		logger.WithField("types", candidateTypes).Debugln("enabling limit of ICE candidate network type")
		s.SetNetworkTypes(candidateTypes)
	}

	if options.Config.ICEEphemeralUDPPortRange[1] != 0 {
		logger.WithFields(logrus.Fields{
			"min": options.Config.ICEEphemeralUDPPortRange[0],
			"max": options.Config.ICEEphemeralUDPPortRange[1],
		}).Debugln("limiting ICE ports")
		if err := s.SetEphemeralUDPPortRange(options.Config.ICEEphemeralUDPPortRange[0], options.Config.ICEEphemeralUDPPortRange[1]); err != nil {
			return nil, fmt.Errorf("failed to set ICE port range: %w", err)
		}
	}

	// TODO(longsleep): Set more settings.

	sfu := &RTMChannelSFU{
		options: options,
		logger:  logger,

		ws: ws,

		webrtcAPI: webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(s)),
		webrtcConfiguration: &webrtc.Configuration{
			ICEServers:   []webrtc.ICEServer{},
			SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
		},
	}

	return sfu, nil
}

func (sfu *RTMChannelSFU) Start(ctx context.Context) error {
	var err error
	errCh := make(chan error, 1)

	sfu.wsCtx, sfu.wsCancel = context.WithCancel(ctx)

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()

		sfu.logger.Infoln("sfu connection established, waiting for action")
		readPumpErr := sfu.readPump()
		if readPumpErr != nil {
			errCh <- readPumpErr
		}
	}()

	select {
	case err = <-errCh:
		// breaks
	}

	sfu.wsCancel()
	wg.Wait()

	return err
}

func (sfu *RTMChannelSFU) readPump() error {
	var mt websocket.MessageType
	var reader io.Reader
	var b *bytes.Buffer
	var err error
	for {
		mt, reader, err = sfu.ws.Reader(sfu.wsCtx)
		if err != nil {
			sfu.logger.WithError(err).Errorln("sfu failed to get reader")
			return err
		}

		b = bpool.Get()
		if _, err = b.ReadFrom(reader); err != nil {
			bpool.Put(b)
			return fmt.Errorf("sfu reader read error: %w", err)
		}

		switch mt {
		case websocket.MessageText:
		default:
			sfu.logger.WithField("message_type", mt).Warnln("sfu received unknown websocket message type")
			continue
		}

		message := &webrtcMessage{}
		err = json.Unmarshal(b.Bytes(), message)
		bpool.Put(b)
		if err != nil {
			sfu.logger.WithError(err).Errorln("sfu websocket message parse error")
			continue
		}

		switch message.Type {
		case "webrtc":
			err = sfu.handleWebRTCMessage(message.RTMTypeWebRTC)

		default:
			sfu.logger.WithField("type", message.RTMTypeEnvelope.Type).Warnln("sfu received unknown rtm message type")
			continue
		}

		if err != nil {
			sfu.logger.WithError(err).Errorln("error while processing sfu websocket message")
			return err
		}
	}
}

func (sfu *RTMChannelSFU) handleWebRTCMessage(message *api.RTMTypeWebRTC) error {
	// TODO(longsleep): Compare message `v` field with our implementation version.
	var err error

	switch message.Subtype {
	case api.RTMSubtypeNameWebRTCChannel:
		sfu.Lock()
		channel := sfu.channel
		if channel != nil {
			sfu.Unlock()
			sfu.logger.WithField("channel", channel.channel).Errorln("sfu channel received but already have channel")
			return errors.New("already have channel")
		}
		channel, err = NewRTMChannelSFUChannel(sfu, message)
		if err != nil {
			sfu.Unlock()
			return fmt.Errorf("failed to create channel: %w", err)
		}
		sfu.channel = channel
		sfu.logger.WithField("channel", sfu.channel.channel).Infoln("sfu channel created")
		sfu.Unlock()

	case api.RTMSubtypeNameWebRTCSignal:
		sfu.RLock()
		channel := sfu.channel
		sfu.RUnlock()
		if channel == nil {
			sfu.logger.WithField("channel", message.Channel).Errorln("sfu has no channel")
			return errors.New("no channel")
		}
		err = channel.handleWebRTCSignalMessage(message)

	default:
		sfu.logger.WithField("subtype", message.Subtype).Warnln("sfu received unknown webrtc message sub type")
	}

	if err != nil {
		sfu.logger.WithError(err).Errorln("error while handling sfu webrtc message")
	}

	return err
}

func (sfu *RTMChannelSFU) Close() error {
	sfu.wsCancel()
	return nil
}

type rtmChannelUserRecord struct {
	channel *RTMChannelSFUChannel

	when time.Time
	id   string

	connections cmap.ConcurrentMap // Holds target connections for the associated user by target.
	senders     cmap.ConcurrentMap // Holds the connection which receive streams..
}

type rtmChannelConnectionRecord struct {
	sync.RWMutex

	owner *rtmChannelUserRecord

	id    string
	rpcid string

	initiator bool
	pipeline  *api.RTMDataWebRTCChannelPipeline

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

	tracks map[string]*webrtc.Track
}

func (record *rtmChannelConnectionRecord) maybeNegotiateAndUnlock() {
	defer record.Unlock()

	select {
	case needsNegotiation := <-record.needsNegotiation:
		if needsNegotiation {
			record.owner.channel.logger.WithField("pcid", record.pcid).Debugln("<<< nnn needs negotiation", record.owner.id)
			if negotiateErr := record.owner.channel.negotiate(record, record.owner, record.state); negotiateErr != nil {
				record.owner.channel.logger.WithError(negotiateErr).Errorln("nnn failed to trigger negotiation, killing sfu channel")
				record.owner.channel.sfu.Close() // NOTE(longsleep): This is brutal, add dedicated errorchannel or similar.
			}
		}
	default:
		// No negotiation required.
	}
}

type rtmChannelConnectionTrack struct {
	track      *webrtc.Track
	source     *rtmChannelUserRecord
	connection *rtmChannelConnectionRecord
}

type RTMChannelSFUChannel struct {
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

	trackCh    chan *rtmChannelConnectionTrack
	receiverCh chan *rtmChannelConnectionRecord
}

func NewRTMChannelSFUChannel(sfu *RTMChannelSFU, message *api.RTMTypeWebRTC) (*RTMChannelSFUChannel, error) {
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

	channel := &RTMChannelSFUChannel{
		when: time.Now(),
		sfu:  sfu,

		logger: sfu.logger.WithField("channel", message.Channel),

		channel:  message.Channel,
		hash:     message.Hash,
		group:    message.Group,
		pipeline: extra.Pipeline,

		connections: cmap.New(),

		trackCh:    make(chan *rtmChannelConnectionTrack, 64),
		receiverCh: make(chan *rtmChannelConnectionRecord, 64),
	}

	go func() {
		// Track channel worker adds newly added tracks to all receivers.
		// TODO(longsleep): Add a way to exit this.
		var logger logrus.FieldLogger
		var index uint64
		for {
			select {
			case trackRecord := <-channel.trackCh:
				index++
				logger = channel.logger.WithFields(logrus.Fields{
					"source": trackRecord.source.id,
					"sfu_a":  index,
				})
				logger.Debugln("ooo got local sfu track to publish")

				track := trackRecord.track

				channel.connections.IterCb(func(target string, record interface{}) {
					logger.Debugln("ooo sfu publishing to", target)
					if target == trackRecord.source.id {
						// Do not publish to self.
						return
					}
					logger = logger.WithField("target", target)

					logger.Debugln("ooo sfu track target")
					targetRecord := record.(*rtmChannelUserRecord)

					var ok bool
					record, ok = targetRecord.connections.Get(trackRecord.source.id)
					if !ok {
						logger.Warnln("ooo publishing sfu track to target which does not have a matching source connection")
						return
					}
					connectionRecord := record.(*rtmChannelConnectionRecord)
					logger.Debugln("ooo sfu using connection", connectionRecord.id)
					connectionRecord.Lock()
					defer connectionRecord.maybeNegotiateAndUnlock()
					pc := connectionRecord.pc
					if pc == nil {
						// No peer connection in this record, skip.
						return
					}

					logger.WithFields(logrus.Fields{
						"track_id":    track.ID(),
						"track_label": track.Label(),
						"track_kind":  track.Kind(),
						"track_type":  track.PayloadType(),
						"track_ssrc":  track.SSRC(),
					}).Debugln("ooo add sfu track to target")
					if _, addErr := pc.AddTrack(track); addErr != nil {
						logger.WithError(addErr).Errorln("ooo add sfu track to target failed")
						return
					}
					connectionRecord.tracks[track.ID()] = track

					if negotiateErr := channel.negotiationNeeded(connectionRecord); negotiateErr != nil {
						logger.WithError(negotiateErr).Errorln("ooo failed to trigger sfu add track negotiation, killing sfu channel")
						channel.sfu.Close() // NOTE(longsleep): This is brutal, add dedicated errorchannel or similar.
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
		//var l logrus.FieldLogger
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
				sourceRecord := record.(*rtmChannelUserRecord)

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
					senderRecord := record.(*rtmChannelConnectionRecord)
					senderRecord.RLock()
					defer senderRecord.RUnlock()
					logger.Debugln("sss sfu using wanted sender source")

					addedTrack := false
					for id, track := range senderRecord.tracks {
						if _, ok := connectionRecord.tracks[id]; ok {
							// Avoid adding the same track multiple times.
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
						}).Debugln("sss add sfu track to target")
						if _, addErr := pc.AddTrack(track); addErr != nil {
							logger.WithError(addErr).Errorln("sss add sfu track to target failed")
							continue
						}
						connectionRecord.tracks[id] = track
						addedTrack = true
					}

					if addedTrack {
						if negotiateErr := channel.negotiationNeeded(connectionRecord); negotiateErr != nil {
							logger.WithError(negotiateErr).Errorln("sss failed to trigger sfu add track negotiation, killing sfu channel")
							channel.sfu.Close() // NOTE(longsleep): This is brutal, add dedicated errorchannel or similar.
							return
						}
					} else {
						logger.Debugln("sss sfu target is already up to date")
					}
				}()

				break

				/*
					channel.connections.IterCb(func(source string, record interface{}) {
						logger.Debugln("sss publishing from", source)
						if source == connectionRecord.owner.id {
							// Do not publish from self.
							logger.Debugln("sss not publising to self", source)
							return
						}

						l = logger.WithField("source", source)

						l.Debugln("sss sfu publishing source")
						sourceRecord := record.(*rtmChannelUserRecord)

						var ok bool
						record, ok = sourceRecord.senders.Get("default")
						if !ok {
							// Skip sources which have no sender.
							logger.Debugln("sss skip publishing, no sender")
							return
						}
						connectionRecord.Lock()
						defer connectionRecord.maybeNegotiateAndUnlock()
						l.Debugln("sss sfu using connection", connectionRecord.id)
						pc := connectionRecord.pc
						if pc == nil {
							// No peer connection in our record, do nothing.
							return
						}
						senderRecord := record.(*rtmChannelConnectionRecord)
						senderRecord.RLock()
						defer senderRecord.RUnlock()
						l.Debugln("sss sfu using sender source")

						addedTrack := false
						for id, track := range senderRecord.tracks {
							if _, ok := connectionRecord.tracks[id]; ok {
								// Avoid adding the same track multiple times.
								continue
							}
							l.WithFields(logrus.Fields{
								"sender":          senderRecord.owner.id,
								"sender_track_id": id,
								"track_id":        track.ID(),
								"track_label":     track.Label(),
								"track_kind":      track.Kind(),
								"track_type":      track.PayloadType(),
								"track_ssrc":      track.SSRC(),
							}).Debugln("sss add sfu track to target")
							if _, addErr := pc.AddTrack(track); addErr != nil {
								l.WithError(addErr).Errorln("sss add sfu track to target failed")
								continue
							}
							connectionRecord.tracks[id] = track
							addedTrack = true
						}

						if addedTrack {
							if negotiateErr := channel.negotiationNeeded(connectionRecord); negotiateErr != nil {
								l.WithError(negotiateErr).Errorln("sss failed to trigger sfu add track negotiation, killing sfu channel")
								channel.sfu.Close() // NOTE(longsleep): This is brutal, add dedicated errorchannel or similar.
								return
							}
						} else {
							l.Debugln("sfu target is already up to date")
						}
					})*/
			}
		}
	}()

	return channel, nil
}

func (channel *RTMChannelSFUChannel) handleWebRTCSignalMessage(message *api.RTMTypeWebRTC) error {
	if message.Channel != channel.channel {
		return fmt.Errorf("channel mismatch, got %v, expected %v", message.Channel, channel.channel)
	}

	var record interface{}
	var err error

	//channel.logger.Debugf("xxx signal from: %v", message.Source)
	record = channel.connections.Upsert(message.Source, nil, func(ok bool, userRecord interface{}, n interface{}) interface{} {
		if !ok {
			//channel.logMessage("xxx trigger new userRecord", message)
			userRecordImpl := &rtmChannelUserRecord{
				channel: channel,

				when: time.Now(),
				id:   message.Source,

				connections: cmap.New(),
				senders:     cmap.New(),
			}
			channel.logger.WithField("source", message.Source).Debugln("new user")

			// Initiate default sender too.
			defaultSenderConnectionRecord := &rtmChannelConnectionRecord{
				owner: userRecordImpl,

				id:       channel.pipeline.Pipeline,
				pipeline: channel.pipeline,
				state:    channel.pipeline.Pipeline,

				requestedTransceivers: &sync.Map{},
				tracks:                make(map[string]*webrtc.Track),
				needsNegotiation:      make(chan bool, 1), // Allow exactly one.
				iceComplete:           make(chan bool),
			}
			userRecordImpl.senders.Set("default", defaultSenderConnectionRecord)
			go func() {
				defaultSenderConnectionRecord.Lock()
				defer defaultSenderConnectionRecord.maybeNegotiateAndUnlock()

				if pc, pcErr := channel.createPeerConnection(defaultSenderConnectionRecord, userRecordImpl, defaultSenderConnectionRecord.state); pcErr != nil {
					channel.logger.WithField("source", message.Source).WithError(pcErr).Errorln("failed to create new sender peer connection: %w", pcErr)
					channel.sfu.Close() // NOTE(longsleep): This is brutal, add dedicated errorchannel or similar.
				} else {
					if experimentAlwaysAddTransceiverToSender {
						// TODO(longsleep): Make recvonly again, investigate if this causes issues.
						/*transceiverInit := webrtc.RtpTransceiverInit{
							Direction: webrtc.RTPTransceiverDirectionRecvonly,
						}*/
						if _, errTransceiver := pc.AddTransceiver(webrtc.RTPCodecTypeAudio); errTransceiver != nil {
							channel.logger.WithError(errTransceiver).Errorln("error adding transceiver for audio")
						}
						if _, errTransceiver := pc.AddTransceiver(webrtc.RTPCodecTypeVideo); errTransceiver != nil {
							channel.logger.WithError(errTransceiver).Errorln("error adding transceiver for video")
						}
					}
					channel.logger.WithFields(logrus.Fields{
						"target":   message.Source,
						"pipeline": defaultSenderConnectionRecord.id,
						"pcid":     defaultSenderConnectionRecord.pcid,
					}).Debugln("uuu trigger default sender create negotiation", userRecordImpl.id)
					// Directly trigger negotiate, for new default sender.
					if negotiateErr := channel.negotiationNeeded(defaultSenderConnectionRecord); negotiateErr != nil {
						channel.logger.WithError(negotiateErr).Errorln("uuu failed to trigger sender negotiation, killing sfu channel")
						channel.sfu.Close() // NOTE(longsleep): This is brutal, add dedicated errorchannel or similar.
					}
				}
			}()

			userRecord = userRecordImpl
		}
		return userRecord
	})
	sourceRecord := record.(*rtmChannelUserRecord)

	logger := channel.logger.WithFields(logrus.Fields{
		"source": message.Source,
		"target": message.Target,
	})

	if message.Target == channel.pipeline.Pipeline {
		record, _ = sourceRecord.senders.Get("default")
	} else {
		record = sourceRecord.connections.Upsert(message.Target, nil, func(ok bool, connectionRecord interface{}, n interface{}) interface{} {
			if !ok {
				initiator := computeInitiator(sourceRecord.id, message.Target)
				connectionRecord = &rtmChannelConnectionRecord{
					owner: sourceRecord,

					id:        message.Target,
					initiator: initiator,
					state:     message.State,

					requestedTransceivers: &sync.Map{},
					tracks:                make(map[string]*webrtc.Track),
					needsNegotiation:      make(chan bool, 1),
					iceComplete:           make(chan bool),
				}
				logger.WithField("initiator", initiator).Debugln("new connection")
			}
			return connectionRecord
		})
	}
	connectionRecord := record.(*rtmChannelConnectionRecord)

	// NOTE(longsleep): For now we keep the connectionRecord locked and do everything
	// synchronized with it here. In the future, certain parts below this point
	// might be improved to run outside of this lock.
	connectionRecord.Lock()
	defer connectionRecord.maybeNegotiateAndUnlock()

	if message.Pcid != connectionRecord.rpcid {
		pc := connectionRecord.pc
		if pc != nil && connectionRecord.rpcid != "" {
			logger.WithFields(logrus.Fields{
				"rpcid_old": connectionRecord.rpcid,
				"rpdic":     message.Pcid,
				"pcid":      connectionRecord.pcid,
			}).Debugln("uuu rpcid has changed, replacing local peer connection")
			connectionRecord.pc = nil
			connectionRecord.pcid = ""
			if closeErr := pc.Close(); closeErr != nil {
				logger.WithField("pcid", connectionRecord.pcid).Warnln("error while closing replaced peer connection")
			}
			connectionRecord.pendingCandidates = nil
			close(connectionRecord.needsNegotiation)
			connectionRecord.needsNegotiation = make(chan bool, 1)
			connectionRecord.queuedNegotiation = false
			connectionRecord.isNegotiating = false
			connectionRecord.iceComplete = make(chan bool)
			connectionRecord.requestedTransceivers = &sync.Map{}
			connectionRecord.tracks = make(map[string]*webrtc.Track)
		}
		connectionRecord.rpcid = message.Pcid
		logger.WithFields(logrus.Fields{
			"pcic":  connectionRecord.pcid,
			"rpcid": message.Pcid,
		}).Debugln("uuu bound connection to remote")
	}

	pcCreated := false
	if connectionRecord.pc == nil {
		if pc, pcErr := channel.createPeerConnection(connectionRecord, sourceRecord, connectionRecord.state); pcErr != nil {
			return fmt.Errorf("uuu failed to create new peer connection: %w", pcErr)
		} else {
			if experimentAlwaysAddTransceiverWhenInitiator && connectionRecord.initiator {
				transceiverInit := webrtc.RtpTransceiverInit{
					Direction: webrtc.RTPTransceiverDirectionSendrecv, // AddTransceiverFromKind currently only supports recvonly and sendrecv (Pion).
				}
				if _, errTransceiver := pc.AddTransceiver(webrtc.RTPCodecTypeVideo, transceiverInit); errTransceiver != nil {
					channel.logger.WithError(errTransceiver).Errorln("uuu error adding transceiver for video")
				}
				if _, errTransceiver := pc.AddTransceiver(webrtc.RTPCodecTypeAudio, transceiverInit); err != nil {
					channel.logger.WithError(errTransceiver).Errorln("uuu error adding transceiver for audio")
				}
			}
		}
		channel.receiverCh <- connectionRecord
		pcCreated = true
	}

	signal := &RTMDataWebRTCSignal{}
	if err = json.Unmarshal(message.Data, signal); err != nil {
		return fmt.Errorf("failed to parse signal data: %w", err)
	}

	found := false

	if signal.Renegotiate {
		found = true
		if connectionRecord.initiator && (!signal.Noop || pcCreated) {
			logger.WithField("pcid", connectionRecord.pcid).Debugln("uuu trigger received renegotiate negotiation ", sourceRecord.id)
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
		if connectionRecord.pc.RemoteDescription() != nil {
			if err = connectionRecord.pc.AddICECandidate(candidate); err != nil {
				return fmt.Errorf("failed to add ice candidate: %w", err)
			}
		} else {
			connectionRecord.pendingCandidates = append(connectionRecord.pendingCandidates, &candidate)
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
		haveRemoteDescription := connectionRecord.pc.CurrentRemoteDescription() != nil
		if haveRemoteDescription {
			logger.Debugln(">>> sdp signal while already having remote description set")
			for {
				// NOTE(longsleep): This is a workaround for the problem when the remote descriptions is overwritten
				// while the underlaying DTLS transport has not started. If this is the best solution or if it better
				// be detected / avoided somewhere else remains to be seen. For now, this seems to cure the problem.
				wait := false
				for _, sender := range connectionRecord.pc.GetSenders() {
					senderState := sender.Transport().State()
					logger.Debugln(">>> sdp sender transport state", senderState)
					if senderState == webrtc.DTLSTransportStateNew {
						wait = true
						break
					}
				}
				if wait {
					logger.Debugln(">>> sdp sender transport not started yet, wait a bit")
					select {
					case <-time.After(100 * time.Millisecond):
						// breaks
					}
				} else {
					break
				}
			}
		}
		if err = connectionRecord.pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: sdpType,
			SDP:  sdpString,
		}); err != nil {
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
			logger.WithField("pcid", connectionRecord.pcid).Debugln("offer received from initiator, creating answer")
			if err = channel.createAnswer(connectionRecord, sourceRecord, connectionRecord.state); err != nil {
				return fmt.Errorf("failed to send answer for offer: %w", err)
			}
		}
	}

	if len(signal.TransceiverRequest) > 0 {
		found = true
		// TODO(longsleep): XXX
		channel.logMessage("uuu transceiver request", message)
		if connectionRecord.initiator {
			var transceiverRequest RTMDataTransceiverRequest
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

func (channel *RTMChannelSFUChannel) logMessage(text string, message interface{}) {
	b, _ := json.MarshalIndent(message, "", "  ")
	channel.logger.Debugln(text, string(b))
}

func (channel *RTMChannelSFUChannel) send(message interface{}) error {
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

func (channel *RTMChannelSFUChannel) createPeerConnection(connectionRecord *rtmChannelConnectionRecord, sourceRecord *rtmChannelUserRecord, state string) (*webrtc.PeerConnection, error) {
	pc, err := channel.sfu.webrtcAPI.NewPeerConnection(*channel.sfu.webrtcConfiguration)
	if err != nil {
		return nil, fmt.Errorf("error creating peer connection: %w", err)
	}

	pcid := rndm.GenerateRandomString(7)

	logger := channel.logger.WithFields(logrus.Fields{
		"source": sourceRecord.id,
		"target": connectionRecord.id,
		"pcid":   pcid,
	})

	// TODO(longsleep): Create data channel when initiator.
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

	pc.OnDataChannel(func(dataChannel *webrtc.DataChannel) {
		logger.Debugln("ddd data channel received")
		dataChannelErr := channel.setupDataChannel(connectionRecord, dataChannel)
		if dataChannelErr != nil {
			logger.WithError(dataChannelErr).Errorln("ddd error setting up remote data channel")
		}
	})

	// TODO(longsleep): Bind all event handlers to pcid.
	pc.OnSignalingStateChange(func(signalingState webrtc.SignalingState) {
		logger.Debugln("onSignalingStateChange", signalingState)
		if signalingState == webrtc.SignalingStateStable {
			connectionRecord.Lock()
			defer connectionRecord.maybeNegotiateAndUnlock()
			if connectionRecord.isNegotiating {
				connectionRecord.isNegotiating = false
				logger.Debugln("nnn negotiation complete")
				if connectionRecord.queuedNegotiation {
					connectionRecord.queuedNegotiation = false
					logger.WithField("pcid", connectionRecord.pcid).Debugln("nnn trigger queued negotiation")
					if negotiationErr := channel.negotiationNeeded(connectionRecord); negotiationErr != nil {
						logger.WithError(negotiationErr).Errorln("nnn failed to trigger queued negotiation, killing sfu channel")
						channel.sfu.Close() // NOTE(longsleep): This is brutal, add dedicated errorchannel or similar.
					}
				}
			}
		}
	})
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		//logger.Debugln("onICECandidate", candidate)
		var candidateInitP *webrtc.ICECandidateInit
		if candidate != nil {
			candidateInit := candidate.ToJSON()
			candidateInitP = &candidateInit
		} else {
			// ICE complete.
			logger.Debugln("ICE complete")
			close(connectionRecord.iceComplete)
			return
		}

		connectionRecord.Lock()
		defer connectionRecord.Unlock()
		if candidateErr := channel.sendCandidate(connectionRecord, sourceRecord, state, candidateInitP); candidateErr != nil {
			logger.WithError(candidateErr).Errorln("failed to send candidate, killing sfu channel")
			channel.sfu.Close() // NOTE(longsleep): This is brutal, add dedicated errorchannel or similar.
		}
	})
	pc.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {
		logger.Debugln("onConnectionStateChange", connectionState)
	})
	pc.OnTrack(func(remoteTrack *webrtc.Track, receiver *webrtc.RTPReceiver) {
		logger.WithFields(logrus.Fields{
			"track_id":    remoteTrack.ID(),
			"track_label": remoteTrack.Label(),
			"track_kind":  remoteTrack.Kind(),
			"track_type":  remoteTrack.PayloadType(),
			"track_ssrc":  remoteTrack.SSRC(),
		}).Debugln("ooo onTrack")
		connectionRecord.Lock()
		if connectionRecord.pipeline == nil {
			connectionRecord.Unlock()
			logger.Warnln("ooo received a track but connection is no pipeline, ignoring track")
			return
		}

		if connectionRecord.guid == "" {
			//connectionRecord.guid = newRandomGUID()
			connectionRecord.guid = "stream_" + connectionRecord.owner.id
		}

		if remoteTrack.PayloadType() == webrtc.DefaultPayloadTypeVP8 || remoteTrack.PayloadType() == webrtc.DefaultPayloadTypeVP9 || remoteTrack.PayloadType() == webrtc.DefaultPayloadTypeH264 {
			// Video.
			if _, ok := connectionRecord.tracks["video"]; ok {
				// XXX(longsleep): What to do here?
				logger.Warnln("ooo received a video track but already got one, ignoring track")
				return
			}

			//trackID := newRandomGUID()
			trackID := "vtrack_" + connectionRecord.owner.id
			videoTrack, trackErr := pc.NewTrack(remoteTrack.PayloadType(), remoteTrack.SSRC(), trackID, connectionRecord.guid)
			if trackErr != nil {
				logger.WithError(trackErr).WithField("label", remoteTrack.Label()).Errorln("ooo failed to create new track for video")
				return
			}
			connectionRecord.tracks["video"] = videoTrack
			connectionRecord.Unlock()

			// Make track available for forwarding.
			channel.trackCh <- &rtmChannelConnectionTrack{
				track:      videoTrack,
				connection: connectionRecord,
				source:     sourceRecord,
			}

			// Launch helpers.
			rtcpPLIInterval := time.Second * 3 // TODO(longsleep): Move to const, and figure out good value.
			go func() {
				ticker := time.NewTicker(rtcpPLIInterval)
				var writeErr error
				for range ticker.C {
					writeErr = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: videoTrack.SSRC()}})
					if writeErr != nil {
						logger.WithError(writeErr).WithField("label", remoteTrack.Label()).Errorln("ooo failed to write picture loss indicator")
					}
				}
			}()

			// Pump track data.
			rtpBuf := make([]byte, 1400)
			var readN int
			var readWriteErr error
			for {
				readN, readWriteErr = remoteTrack.Read(rtpBuf)
				if readWriteErr != nil {
					logger.WithError(readWriteErr).WithField("label", remoteTrack.Label()).Errorln("ooo failed to read from video track")
					break
				}

				_, readWriteErr = videoTrack.Write(rtpBuf[:readN])
				if readWriteErr != nil && readWriteErr != io.ErrClosedPipe { // ErrClosedPipe means we don't have any subscribers.
					logger.WithError(readWriteErr).WithField("label", remoteTrack.Label()).Errorln("ooo failed to write to video track")
					break
				}
			}

		} else {
			// Audio.
			if _, ok := connectionRecord.tracks["audio"]; ok {
				// XXX(longsleep): What to do here?
				logger.Warnln("ooo received a audio track but already got one, ignoring track")
				return
			}

			trackID := "atrack_" + connectionRecord.owner.id
			audioTrack, trackErr := pc.NewTrack(remoteTrack.PayloadType(), remoteTrack.SSRC(), trackID, connectionRecord.guid)
			if trackErr != nil {
				logger.WithError(trackErr).WithField("label", remoteTrack.Label()).Errorln("ooo failed to create new track for audio")
				return
			}
			connectionRecord.tracks["audio"] = audioTrack
			connectionRecord.Unlock()

			// Make track available for forwarding.
			channel.trackCh <- &rtmChannelConnectionTrack{
				track:      audioTrack,
				connection: connectionRecord,
				source:     sourceRecord,
			}

			// Pump track data.
			rtpBuf := make([]byte, 1400)
			var readN int
			var readWriteErr error
			for {
				readN, readWriteErr = remoteTrack.Read(rtpBuf)
				if readWriteErr != nil {
					logger.WithError(readWriteErr).WithField("label", remoteTrack.Label()).Errorln("ooo failed to read from audio track")
					break
				}

				_, readWriteErr = audioTrack.Write(rtpBuf[:readN])
				if readWriteErr != nil && readWriteErr != io.ErrClosedPipe { // ErrClosedPipe means we don't have any subscribers.
					logger.WithError(readWriteErr).WithField("label", remoteTrack.Label()).Errorln("ooo failed to write to audio track")
					break
				}
			}
		}
	})

	logger.WithFields(logrus.Fields{
		"initiator": connectionRecord.initiator,
	}).Debugln("uuu created new peer connection")

	connectionRecord.pc = pc
	connectionRecord.pcid = pcid
	connectionRecord.isNegotiating = false

	if connectionRecord.initiator {
		logger.Debugln("uuu trigger initiator negotiation")
		if err = channel.negotiationNeeded(connectionRecord); err != nil {
			return nil, fmt.Errorf("failed to schedule negotiation: %w", err)
		}
	}

	return pc, nil
}

func (channel *RTMChannelSFUChannel) setupDataChannel(connectionRecord *rtmChannelConnectionRecord, dataChannel *webrtc.DataChannel) error {
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

func (channel *RTMChannelSFUChannel) createOffer(connectionRecord *rtmChannelConnectionRecord, sourceRecord *rtmChannelUserRecord, state string) error {
	sessionDescription, err := connectionRecord.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("failed to create offer: %w", err)
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
		State:   state,
		Pcid:    connectionRecord.pcid,
		Data:    sdpBytes,
	}

	channel.logger.WithField("pcid", connectionRecord.pcid).Debugln(">>> sending offer", sourceRecord.id)
	return channel.send(out)
}

func (channel *RTMChannelSFUChannel) createAnswer(connectionRecord *rtmChannelConnectionRecord, sourceRecord *rtmChannelUserRecord, state string) error {
	sessionDescription, err := connectionRecord.pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("failed to create answer: %w", err)
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
		State:   state,
		Pcid:    connectionRecord.pcid,
		Data:    sdpBytes,
	}

	go func() {
		if !experimentICETrickle {
			<-connectionRecord.iceComplete
		}

		connectionRecord.Lock()
		defer connectionRecord.maybeNegotiateAndUnlock()

		channel.logger.WithField("pcid", connectionRecord.pcid).Debugln(">>> sending answer", sourceRecord.id)
		if sendErr := channel.send(out); sendErr != nil {
			channel.logger.WithError(sendErr).Errorln("failed to send answer description, killing sfu channel")
			channel.sfu.Close() // NOTE(longsleep): This is brutal, add dedicated errorchannel or similar.
		}

		if !connectionRecord.initiator && connectionRecord.pipeline == nil {
			if transceiversErr := channel.requestMissingTransceivers(connectionRecord, sourceRecord, state); transceiversErr != nil {
				channel.logger.WithError(transceiversErr).Errorln("failed to request missing transceivers, killing sfu channel")
				channel.sfu.Close() // NOTE(longsleep): This is brutal, add dedicated errorchannel or similar.
			}
		}
	}()

	return nil
}

func (channel *RTMChannelSFUChannel) negotiationNeeded(connectionRecord *rtmChannelConnectionRecord) error {
	//connectionRecord.needsNegotiation = true
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

func (channel *RTMChannelSFUChannel) negotiate(connectionRecord *rtmChannelConnectionRecord, sourceRecord *rtmChannelUserRecord, state string) error {
	if connectionRecord.initiator {
		if connectionRecord.isNegotiating {
			connectionRecord.queuedNegotiation = true
			channel.logger.Debugln("nnn initiator already negotiating, queueing")
		} else {
			channel.logger.WithFields(logrus.Fields{
				"target": sourceRecord.id,
				"source": connectionRecord.id,
				"pcid":   connectionRecord.pcid,
			}).Debugln("nnn start negotiation, sending offer")
			if err := channel.createOffer(connectionRecord, sourceRecord, state); err != nil {
				return fmt.Errorf("failed to create offer in negotiate: %w", err)
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
			renegotiate := &RTMDataWebRTCSignal{
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

func (channel *RTMChannelSFUChannel) requestMissingTransceivers(connectionRecord *rtmChannelConnectionRecord, sourceRecord *rtmChannelUserRecord, state string) error {
	logger := channel.logger.WithFields(logrus.Fields{
		"target": connectionRecord.id,
		"source": sourceRecord.id,
		"pcid":   connectionRecord.pcid,
	})
	logger.Debugln("uuu requestMissingTransceivers")
	for _, transceiver := range connectionRecord.pc.GetTransceivers() {
		sender := transceiver.Sender()
		logger.Debugln("uuu requestMissingTransceiver has sender", sender != nil)
		if sender == nil {
			continue
		}
		track := sender.Track()
		logger.Debugln("uuu requestMissingTransceiver sender has track", track != nil)
		if track == nil {
			continue
		}
		// Avoid adding the same transceiver multiple times.
		if _, seen := connectionRecord.requestedTransceivers.LoadOrStore(transceiver, nil); seen {
			logger.Debugln("uuu requestMissingTransceiver already requested, doing nothing")
			continue
		}
		channel.logger.Debugf("uuu requestMissingTransceivers for transceiver", track.Kind())
		if err := channel.addTransceiver(connectionRecord, sourceRecord, state, track.Kind(), nil); err != nil {
			return fmt.Errorf("uuu error while adding missing transceiver: %w", err)
		}
	}
	/*if !connectionRecord.initiator {
		if _, seen := connectionRecord.requestedTransceivers.LoadOrStore(globalAudioTransceiver, nil); !seen {
			logger.Debugf("uuu requestMissingTransceivers add global audio transceiver")
			if _, err := connectionRecord.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
				return fmt.Errorf("uuu error while adding missing global audio transceiver: %w", err)
			}
		} else {
			logger.Debugln("uuu global audio transceiver already added")
		}
		if _, seen := connectionRecord.requestedTransceivers.LoadOrStore(globalVideoTransceiver, nil); !seen {
			logger.Debugf("uuu requestMissingTransceivers add global video transceiver")
			if _, err := connectionRecord.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
				return fmt.Errorf("uuu error while adding missing global video transceiver: %w", err)
			}
		} else {
			logger.Debugln("uuu global video transceiver already added")
		}
	}*/

	return nil
}

func (channel *RTMChannelSFUChannel) addTransceiver(connectionRecord *rtmChannelConnectionRecord, sourceRecord *rtmChannelUserRecord, state string, kind webrtc.RTPCodecType, init *webrtc.RtpTransceiverInit) error {
	if connectionRecord.initiator {
		initArray := []webrtc.RtpTransceiverInit{}
		if init != nil {
			initArray = append(initArray, *init)
		}
		if _, err := connectionRecord.pc.AddTransceiverFromKind(kind, initArray...); err != nil {
			return fmt.Errorf("uuu failed to add transceiver: %w", err)
		}
		channel.logger.WithField("pcid", connectionRecord.pcid).Debugln("uuu trigger negotiation after add transceiver", kind)
		if err := channel.negotiationNeeded(connectionRecord); err != nil {
			return fmt.Errorf("uuu failed to schedule negotiation: %w", err)
		}
	} else {
		if !experimentAddTransceiver {
			channel.logger.Debugln("uuu addTransceiver experiment not enabled")
			return nil
		}
		channel.logger.WithFields(logrus.Fields{
			"target": sourceRecord.id,
			"source": connectionRecord.id,
		}).Debugln("uuu requesting transceivers from initiator")
		transceiverRequest := &RTMDataTransceiverRequest{
			Kind: kind.String(),
		}
		transceiverRequestBytes, err := json.MarshalIndent(transceiverRequest, "", "\t")
		if err != nil {
			return fmt.Errorf("uuu failed to mashal transceiver request: %w", err)
		}
		transceiverRequestData := &RTMDataWebRTCSignal{
			TransceiverRequest: transceiverRequestBytes,
		}
		transceiverRequestDataBytes, err := json.MarshalIndent(transceiverRequestData, "", "\t")
		if err != nil {
			return fmt.Errorf("uuu failed to mashal transceiver request data: %w", err)
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
			State:   state,
			Pcid:    connectionRecord.pcid,
			Data:    transceiverRequestDataBytes,
		}
		channel.logger.Debugln(">>> uuu sending transceiver request", sourceRecord.id)
		if err = channel.send(out); err != nil {
			return fmt.Errorf("uuu failed to send transceiver request: %w", err)
		}
	}

	return nil
}

func (channel *RTMChannelSFUChannel) sendCandidate(connectionRecord *rtmChannelConnectionRecord, sourceRecord *rtmChannelUserRecord, state string, init *webrtc.ICECandidateInit) error {
	candidateBytes, err := json.MarshalIndent(init, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to mashal candidate: %w", err)
	}
	candidateData := &RTMDataWebRTCSignal{
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
