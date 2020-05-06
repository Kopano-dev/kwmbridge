/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"net/http"
	"time"

	"github.com/justinas/alice"
	"github.com/pion/webrtc/v2"
	kwmapi "stash.kopano.io/kwm/kwmserver/signaling/api-v1"

	api "stash.kopano.io/kwm/kwmbridge/bridge/api-v0"
	"stash.kopano.io/kwm/kwmbridge/internal/jitterbuffer"
)

func (sfu *RTMChannelSFU) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	sfu.router.ServeHTTP(rw, req)
}

func (sfu *RTMChannelSFU) addRoutes() {
	r := sfu.router
	chain := alice.New()

	r.Handle("/", chain.ThenFunc(sfu.HTTPRootHandler))
	r.Handle("/channel", chain.ThenFunc(sfu.HTTPChannelHandler))
	r.Handle("/channel/users", chain.ThenFunc(sfu.HTTPChannelUsersHandler))
	r.Handle("/channel/users/{userID}", chain.ThenFunc(sfu.HTTPChannelUsersHandler))
	r.Handle("/channel/users/{userID}/{actionID:(?:publishers|connections)}", chain.ThenFunc(sfu.HTTPChannelUsersHandler))

	// /api/kwm/v0/bridge/mcuc/clients/:clientid/attached/:transaction/rtmcsfu/channel
	// /api/kwm/v0/bridge/mcuc/clients/:clientid/attached/:transaction/rtmcsfu/channel/users
	// /api/kwm/v0/bridge/mcuc/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/publishers
	// /api/kwm/v0/bridge/mcuc/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/connections
	// /api/kwm/v0/bridge/mcuc/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/connections/:connectionid/tracks
	// /api/kwm/v0/bridge/mcuc/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/connections/:connectionid/publishers
	// /api/kwm/v0/bridge/mcuc/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/connections/:connectionid/pending
	// /api/kwm/v0/bridge/mcuc/clients/:clientid/attached/:transaction/rtmcsfu/channel/users/:userid/connections/:connectionid/p2p/connections

	return
}

func (sfu *RTMChannelSFU) HTTPRootHandler(rw http.ResponseWriter, req *http.Request) {
	summary := sfu.Summary()

	resource := &RootResource{}
	if summary != nil {
		resource.HasChannel = true
		resource.Summary = summary
	}

	if writeErr := api.WriteResourceAsItemResourceResponseJSON(rw, req, resource); writeErr != nil {
		sfu.logger.WithError(writeErr).Errorln("failed to write json response")
	}
}

type RootResource struct {
	HasChannel bool        `json:"hasChannel"`
	Summary    interface{} `json:"summary"`
}

func (sfu *RTMChannelSFU) HTTPChannelHandler(rw http.ResponseWriter, req *http.Request) {
	sfu.RLock()
	defer sfu.RUnlock()

	channel := sfu.getChannelResourceOrWriteError(rw)
	if channel == nil {
		return
	}

	channel.RLock()
	defer channel.RUnlock()

	resource := &ChannelResource{
		When:    channel.when,
		Channel: channel.channel,
		Hash:    channel.hash,
		Group:   channel.group,

		Pipeline: channel.pipeline,

		UserCount: uint64(channel.connections.Count()),
	}

	if writeErr := api.WriteResourceAsItemResourceResponseJSON(rw, req, resource); writeErr != nil {
		sfu.logger.WithError(writeErr).Errorln("failed to write json response")
	}
}

func (sfu *RTMChannelSFU) getChannelResourceOrWriteError(rw http.ResponseWriter) *Channel {
	channel := sfu.channel
	if channel == nil {
		if writeErr := api.WriteErrorAsJSON(rw, api.NewErrorWithCodeAndMessage(
			"ErrorMessageNoChannel",
			"The specified bridge has no channel",
			api.ErrNotFound,
		)); writeErr != nil {
			sfu.logger.WithError(writeErr).Errorln("failed to write json error")
		}
	}

	return channel
}

type ChannelResource struct {
	When    time.Time `json:"when"`
	Channel string    `json:"channel"`
	Hash    string    `json:"hash"`
	Group   string    `json:"group"`

	Pipeline *kwmapi.RTMDataWebRTCChannelPipeline `json:"pipeline"`

	UserCount uint64 `json:"userCount"`
}

func (sfu *RTMChannelSFU) HTTPChannelUsersHandler(rw http.ResponseWriter, req *http.Request) {
	userID, _ := api.GetRequestVar(req, "userID")

	sfu.RLock()
	defer sfu.RUnlock()

	channel := sfu.getChannelResourceOrWriteError(rw)
	if channel == nil {
		return
	}

	var resource interface{}
	if userID == "" {
		users := make([]*ChannelUserResource, 0)
		channel.connections.IterCb(func(key string, v interface{}) {
			userRecord := v.(*UserRecord)
			users = append(users, NewChannelUserResource(key, userRecord))
		})

		resource = api.NewCollectionResource(users, req, nil)
	} else {
		user, found := channel.connections.Get(userID)
		if !found {
			if writeErr := api.WriteErrorAsJSON(rw, api.NewErrorWithCodeAndMessage(
				"ErrorMessageUserNotFoundInChannel",
				"The specified user does not existin in the selected channel",
				api.ErrNotFound,
			)); writeErr != nil {
				sfu.logger.WithError(writeErr).Errorln("failed to write json error")
			}
			return
		}

		userRecord := user.(*UserRecord)

		actionID, _ := api.GetRequestVar(req, "actionID")
		switch actionID {
		case "":
			resource = NewChannelUserResource(userID, userRecord)

		case "publishers":
			publishers := make([]*ChannelConnectionResource, 0)
			userRecord.publishers.IterCb(func(key string, v interface{}) {
				publisherRecord := v.(*ConnectionRecord)
				publisherRecord.RLock()
				publishers = append(publishers, NewChannelConnectionResource(key, publisherRecord))
				publisherRecord.RUnlock()
			})
			resource = api.NewCollectionResource(publishers, req, nil)

		case "connections":
			connections := make([]*ChannelConnectionResource, 0)
			userRecord.connections.IterCb(func(key string, v interface{}) {
				connectionRecord := v.(*ConnectionRecord)
				connectionRecord.RLock()
				connections = append(connections, NewChannelConnectionResource(key, connectionRecord))
				connectionRecord.RUnlock()
			})
			resource = api.NewCollectionResource(connections, req, nil)

		default:
			rw.WriteHeader(http.StatusNotFound)
			return
		}

	}

	if writeErr := api.WriteResourceAsJSON(rw, resource); writeErr != nil {
		sfu.logger.WithError(writeErr).Errorln("failed to write json response")
	}
}

type ChannelUserResource struct {
	Key string `json:"key"`

	When time.Time `json:"when"`
	ID   string    `json:"id"`

	IsClosed         bool   `json:"isClosed"`
	ConnectionsCount uint64 `json:"connectionsCount"`
	PublishersCount  uint64 `json:"publishersCount"`
}

func NewChannelUserResource(key string, userRecord *UserRecord) *ChannelUserResource {
	if userRecord == nil {
		return nil
	}

	return &ChannelUserResource{
		Key: key,

		When: userRecord.when,
		ID:   userRecord.id,

		ConnectionsCount: uint64(userRecord.connections.Count()),
		PublishersCount:  uint64(userRecord.publishers.Count()),
		IsClosed:         userRecord.isClosed(),
	}
}

type ChannelConnectionResource struct {
	Key string `json:"key"`

	Owner *ChannelUserResource `json:"owner"`
	Bound *ChannelUserResource `json:"bound"`

	ID    string `json:"id"`
	RPCID string `json:"rpcid"`

	IsInitiator bool `json:"isInitiator"`

	PeerConnection *ConnectionPeerConnectionResource `json:"peerConnection"`
	PCID           string                            `json:"pcid"`
	State          string                            `json:"state"`

	PendingCandidatesCount uint64 `json:"pendingCandidatesCount"`

	HasQueuedNegotiation bool `json:"hasQueuedNegotiation"`
	IsNegotiating        bool `json:"isNegotiating"`

	GUID string `json:"guid"`

	Tracks    map[uint32]interface{} `json:"tracks"`
	Senders   map[uint32]interface{} `json:"senders"`
	Receivers map[uint32]interface{} `json:"receivers"`
	Pending   map[uint32]interface{} `json:"pending"`

	RTPPayloadTypes map[string]uint8 `json:"rtpPayloadTypes"`

	JitterBuffer interface{} `json:"jitterBuffer"`
}

func NewChannelConnectionResource(key string, connectionRecord *ConnectionRecord) *ChannelConnectionResource {
	if connectionRecord == nil {
		return nil
	}

	resource := &ChannelConnectionResource{
		Key: key,

		Owner: NewChannelUserResource("", connectionRecord.owner),
		Bound: NewChannelUserResource("", connectionRecord.bound),

		ID:    connectionRecord.id,
		RPCID: connectionRecord.rpcid,

		IsInitiator: connectionRecord.initiator,

		PeerConnection: NewConnectionPeerConnectionResource(connectionRecord.pc),
		PCID:           connectionRecord.pcid,
		State:          connectionRecord.state,

		PendingCandidatesCount: uint64(len(connectionRecord.pendingCandidates)),

		HasQueuedNegotiation: connectionRecord.queuedNegotiation,
		IsNegotiating:        connectionRecord.isNegotiating,

		GUID: connectionRecord.guid,

		Tracks:    make(map[uint32]interface{}),
		Senders:   make(map[uint32]interface{}),
		Receivers: make(map[uint32]interface{}),
		Pending:   make(map[uint32]interface{}),

		RTPPayloadTypes: connectionRecord.rtpPayloadTypes,

		JitterBuffer: NewJitterBufferResource(connectionRecord.jitterBuffer),
	}

	for key, track := range connectionRecord.tracks {
		resource.Tracks[key] = NewConnectionTrackResource(track)
	}
	for key, rtpSender := range connectionRecord.senders {
		resource.Senders[key] = &ConnectionRTPTransceiverStateResource{
			DTLSTransport: NewDTLSTransportResource(rtpSender.Transport()),
			Track:         NewConnectionTrackResource(rtpSender.Track()),
		}
		rtpSender.Transport().State()
	}
	for key, rtpReceiver := range connectionRecord.receivers {
		resource.Receivers[key] = &ConnectionRTPTransceiverStateResource{
			DTLSTransport: NewDTLSTransportResource(rtpReceiver.Transport()),
			Track:         NewConnectionTrackResource(rtpReceiver.Track()),
		}
	}
	for key, pending := range connectionRecord.pending {
		resource.Pending[key] = &ConnectionTrackRecordResource{
			Connection:      NewChannelConnectionResource(string(key), pending.connection),
			Source:          NewChannelUserResource(string(key), pending.source),
			Track:           NewConnectionTrackResource(pending.track),
			IsRemove:        pending.remove,
			WithTransceiver: pending.transceiver,
		}
	}

	return resource
}

type ConnectionPeerConnectionResource struct {
	ID string `json:"id"`

	ConnectionState   string `json:"connectionState"`
	SignalingState    string `json:"signalingState"`
	ICEGatheringState string `json:"iceGatheringState"`

	LocalDescription  *webrtc.SessionDescription `json:"localDescription"`
	RemoteDescription *webrtc.SessionDescription `json:"remoteDescription"`

	Stats *webrtc.StatsReport `json:"stats"`
}

func NewConnectionPeerConnectionResource(pc *PeerConnection) *ConnectionPeerConnectionResource {
	if pc == nil {
		return nil
	}

	stats := pc.GetStats()

	return &ConnectionPeerConnectionResource{
		ID: pc.ID(),

		ConnectionState:   pc.ConnectionState().String(),
		SignalingState:    pc.SignalingState().String(),
		ICEGatheringState: pc.ICEGatheringState().String(),

		LocalDescription:  pc.LocalDescription(),
		RemoteDescription: pc.RemoteDescription(),

		Stats: &stats,
	}
}

type ConnectionTrackResource struct {
	ID          string `json:"id"`
	SSRC        uint32 `json:"ssrc"`
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	PayloadType uint8  `json:"payloadType"`
}

func NewConnectionTrackResource(track *webrtc.Track) *ConnectionTrackResource {
	if track == nil {
		return nil
	}

	return &ConnectionTrackResource{
		ID:          track.ID(),
		SSRC:        track.SSRC(),
		Kind:        track.Kind().String(),
		Label:       track.Label(),
		PayloadType: track.PayloadType(),
	}
}

type ConnectionRTPTransceiverStateResource struct {
	DTLSTransport *DTLSTransportResource   `json:"dtlsTransport"`
	Track         *ConnectionTrackResource `json:"track"`
}

type DTLSTransportResource struct {
	State      string                `json:"state"`
	Parameters webrtc.DTLSParameters `json:"parameters"`

	ICETransport *webrtc.ICETransport `json:"iceTransport"`
}

func NewDTLSTransportResource(transport *webrtc.DTLSTransport) *DTLSTransportResource {
	if transport == nil {
		return nil
	}

	parameters, _ := transport.GetLocalParameters()

	return &DTLSTransportResource{
		State:      transport.State().String(),
		Parameters: parameters,

		ICETransport: transport.ICETransport(),
	}
}

type ConnectionTrackRecordResource struct {
	Connection      *ChannelConnectionResource `json:"connection"`
	Source          *ChannelUserResource       `json:"source"`
	Track           *ConnectionTrackResource   `json:"track"`
	IsRemove        bool                       `json:"IsRemove"`
	WithTransceiver bool                       `json:"WithTransceiver"`
}

type JitterBufferResource struct {
	ID      string                                 `json:"id"`
	Buffers map[uint32]*JitterBufferBufferResource `json:"buffers"`
}

func NewJitterBufferResource(jitterBuffer *jitterbuffer.JitterBuffer) *JitterBufferResource {
	if jitterBuffer == nil {
		return nil
	}

	buffers := make(map[uint32]*JitterBufferBufferResource)
	for ssrc, b := range jitterBuffer.GetBuffers() {
		lostRate, byteRate := b.GetCurrentRates()
		buffers[ssrc] = &JitterBufferBufferResource{
			IsVideo:     b.IsVideo(),
			PayloadType: b.GetPayloadType(),

			LostRate:  float64(lostRate) / 100,
			Bandwidth: byteRate * 8 / 1000, // Kbps
		}
	}

	return &JitterBufferResource{
		ID:      jitterBuffer.ID(),
		Buffers: buffers,
	}
}

type JitterBufferBufferResource struct {
	IsVideo     bool  `json:"isVideo"`
	PayloadType uint8 `json:"payloadType"`

	LostRate  float64 `json:"lostRate"`
	Bandwidth uint64  `json:"bandwidth"`
}
