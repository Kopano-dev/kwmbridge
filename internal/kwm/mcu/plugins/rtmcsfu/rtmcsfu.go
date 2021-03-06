/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gorilla/mux"
	"github.com/pion/webrtc/v2"
	"github.com/sasha-s/go-deadlock"
	"github.com/sirupsen/logrus"
	"nhooyr.io/websocket"
	api "stash.kopano.io/kwm/kwmserver/signaling/api-v1"

	"stash.kopano.io/kwm/kwmbridge/internal/bpool"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
	"stash.kopano.io/kwm/kwmbridge/internal/kwm/mcu"
)

// Experiments for inccomplete features.
const (
	experimentAddTransceiver               = true
	experimentAlwaysAddTransceiverToSender = false
	experimentICETrickle                   = false // Buggy in pion/webrtc when using multiple answer/offers.
	experimentUseRTCFBNack                 = true
	experimentUseRTCFBTransportCC          = false // Causes bad video frame rate, not implemeneted?
	experimentUseReplayProtection          = false // Causes lot of replay log messages on info level.
	experimentLogSDP                       = false // Logs all SDP in and out
)

const (
	maxChSize = 100
	bridgeID  = "rtmcsfu"
)

const (
	senderTrackVideo uint32 = iota + 1
	senderTrackAudio
)

/**
* WebRTC payload version. All WebRTC payloads will include this value and
* clients can use it to check if they are compatible with the received
* data. This client will reject all messages which are from received with
* older version than defined here.
 */
const WebRTCPayloadVersion = 20180703

type RTMChannelSFU struct {
	deadlock.RWMutex

	options *mcu.Options
	logger  logrus.FieldLogger

	router *mux.Router

	wsCtx    context.Context
	wsCancel context.CancelFunc
	ws       *websocket.Conn

	channel *Channel

	webrtcSettings      *webrtc.SettingEngine
	webrtcConfiguration *webrtc.Configuration
}

func New(attach *kwm.MCUTypeContainer, ws *websocket.Conn, options *mcu.Options) (mcu.Plugin, error) {
	logger := options.Logger.WithFields(logrus.Fields{
		"bridge":      bridgeID,
		"transaction": attach.Transaction,
		"plugin":      attach.Plugin,
	})

	logger.WithFields(logrus.Fields{
		"ICETrickle":       experimentICETrickle,
		"ICELite":          options.Config.ICELite,
		"ReplayProtection": experimentUseReplayProtection,
	}).Infoln("starting rtm channel sfu")

	s := webrtc.SettingEngine{
		LoggerFactory: &loggerFactory{logger},
	}
	s.SetTrickle(experimentICETrickle)
	s.SetLite(options.Config.ICELite)

	if experimentUseReplayProtection {
		s.SetDTLSReplayProtectionWindow(128)
		s.SetSRTPReplayProtectionWindow(64)
		s.SetSRTCPReplayProtectionWindow(32)
	} else {
		s.DisableSRTPReplayProtection(true)
		s.DisableSRTCPReplayProtection(true)
	}

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

	if len(options.Config.NAT1To1IPs) > 0 {
		logger.WithFields(logrus.Fields{
			"ips":           options.Config.NAT1To1IPs,
			"candidateType": options.Config.NAT1To1CandidateType,
		}).Debugln("using external IP addresses for 1:1 (D)NAT")
		if candidateType, err := webrtc.NewICECandidateType(options.Config.NAT1To1CandidateType); err != nil {
			return nil, fmt.Errorf("failed to parse NAT1to1 ICE candidate type: %w", err)
		} else {
			s.SetNAT1To1IPs(options.Config.NAT1To1IPs, candidateType)
		}
	}

	router := mux.NewRouter()

	// TODO(longsleep): Set more settings.

	sfu := &RTMChannelSFU{
		options: options,
		logger:  logger,

		router: router,

		ws: ws,

		webrtcSettings: &s,
		webrtcConfiguration: &webrtc.Configuration{
			ICEServers:   []webrtc.ICEServer{},
			SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
		},
	}

	sfu.addRoutes()

	return sfu, nil
}

func (sfu *RTMChannelSFU) Bridge() string {
	return bridgeID
}

func (sfu *RTMChannelSFU) Summary() interface{} {
	sfu.RLock()
	defer sfu.RUnlock()

	channel := sfu.channel
	if channel == nil {
		return nil
	}

	return &channel.channel
}

func (sfu *RTMChannelSFU) Start(ctx context.Context) error {
	errCh := make(chan error, 1)

	sfu.wsCtx, sfu.wsCancel = context.WithCancel(ctx)

	go func() {
		sfu.logger.Infoln("sfu connection established")
		readPumpErr := sfu.readPump() // This blocks.
		errCh <- readPumpErr          // Always send result, to unblock cleanup.
	}()
	return <-errCh
}

func (sfu *RTMChannelSFU) readPump() error {
	var mt websocket.MessageType
	var reader io.Reader
	var b *bytes.Buffer
	var err error
	for {
		mt, reader, err = sfu.ws.Reader(sfu.wsCtx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			switch websocket.CloseStatus(err) {
			case websocket.StatusNormalClosure, websocket.StatusGoingAway:
				sfu.logger.WithField("status_code", websocket.CloseStatus(err)).Debugln("sfu connection close")
				return nil
			}
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
		if err != nil {
			bpool.Put(b)
			sfu.logger.WithError(err).Errorln("sfu websocket message parse error")
			continue
		}

		switch message.Type {
		case "webrtc":
			m := &api.RTMTypeWebRTC{}
			if unmarshalErr := json.Unmarshal(b.Bytes(), m); unmarshalErr != nil {
				bpool.Put(b)
				sfu.logger.WithError(unmarshalErr).Errorln("sfu websocket webrtc message parse error")
				continue
			}
			bpool.Put(b)
			err = sfu.handleWebRTCMessage(m)

		default:
			bpool.Put(b)
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
		channel, err = NewChannel(sfu, message)
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
			sfu.logger.WithField("channel", message.Channel).Warnln("sfu got signal but has no channel")
			return errors.New("no channel")
		}
		if signalErr := channel.handleWebRTCSignalMessage(message); signalErr != nil {
			sfu.logger.WithError(signalErr).WithField("channel", message.Channel).Warnln("sfu channel error while signal processing")
		}

	case api.RTMSubtypeNameWebRTCHangup:
		sfu.RLock()
		channel := sfu.channel
		sfu.RUnlock()
		if channel == nil {
			// No channel, we do not care much about hangups.
			return nil
		}
		if hangupErr := channel.handleWebRTCHangupMessage(message); hangupErr != nil {
			sfu.logger.WithError(hangupErr).WithField("channel", message.Channel).Warnln("sfu channel error while hangup processing")
		}

	default:
		sfu.logger.WithField("subtype", message.Subtype).Warnln("sfu received unknown webrtc message sub type")
	}

	if err != nil {
		sfu.logger.WithError(err).Errorln("error while handling sfu webrtc message")
	}

	return err
}

func (sfu *RTMChannelSFU) Close() error {
	sfu.Lock()
	defer sfu.Unlock()

	channel := sfu.channel
	sfu.channel = nil

	sfu.wsCancel()
	if channel != nil {
		sfu.logger.WithField("channel", channel.channel).Infoln("sfu channel stop")
		return channel.Stop()
	}
	return nil
}
