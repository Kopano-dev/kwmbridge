# CHANGELOG

## Unreleased



## v0.1.3 (2020-09-28)

- Add support to run kwmbridged behind 1to1 (D)NAT
- Update curl and ca-certificate to latest version


## v0.1.2 (2020-09-22)

- Update Jenkins reporting plugin from checkstyle to recordIssues


## v0.1.1 (2020-09-22)

- Avoid sending to data channel when none is available
- Add Dockerfile
- Fix typos and add beta comment
- Build with Go 1.14.4


## v0.1.0 (2020-06-03)

- Implement screen sharing support
- Use noop renegotiate instead of call to trigger connection
- Add gears to easily enable SDP logging for debug
- Update WebRTC stack to resolve Firefox recvonly
- Disable wait on DTLS start before setting remote description
- Optimize connection creation behavior
- Allow default sender to be initiator
- Only request target answer when received sdp
- Add sdp logging (commented)
- Update third party dependencies
- Fix firefox recvonly offer support
- Updat third party depdencies
- Cure race condition when killing user
- Improve one side close reconnect behavior
- Reset target connection if track write fails
- No longer close neeed negotiation channels
- No longer always add transceiver to sender
- Only use ICE-lite when enabled
- Reset connection record on WebRTC signaling handling errors


## v0.0.7 (2020-05-11)

- Make errors of channel signal and hangup non fatal


## v0.0.6 (2020-05-11)

- Fix race condition when checking for closed user record
- Ensure to reset connection after removal
- Cure panic in ICE agent
- Increase start bandwidth from 300 to 600 kbps
- Increase minimal bandwidth from 90 to 90 kbps
- Skip pending not added tracks without triggering renegotiation
- Expose peer connection states and stats in REST API
- Add peer connection connectionstate timeout
- Add timeout when sending to channel websocket
- Use upstream pion and update to new behavior
- Update 3rd party dependencies


## v0.0.5 (2020-04-29)

- Improve video bandwidth adjusting and initial value
- Improve deadlock detection settings
- Improve cleanup of unneeded connections
- Improve locking while handling incoming webrtc signaling
- Add option to disable deadlock detector and set deadlock timer to 15s
- Use buffered iterator for channel connections to avoid dead lock
- Improve connection reset and cleanup state
- Unify REST API model keys
- Improve REST API resource models
- Add support to jitterbuffer stats via REST API
- Disable REST API http request log
- Implement REST API for kwm mcu plugins
- Use deadlock detector
- Fix deadlock when adding existing tracks to new pc
- Add REST api base
- Implement KWM P2P protocol on data channel


## v0.0.4 (2020-04-02)

- Increase websocket size limit to match kwmserver
- Add bunch of settings to configuration file


## v0.0.3 (2020-04-02)

- Stop all connections to a channel on its close
- Clean up plugin resources completely on detach
- Improve replay detection experiment
- Update webrtc stack to cure race on pc close


## v0.0.2 (2020-03-31)

- Do not log nack misses
- Improve recovery from one sided connection loss
- Ignore target connections without attached user in track pump
- Reduce amout of debug logging
- Add lock when resetting target connections
- Fix race when owner gets removed
- WIP: improve nack situation
- Fix possible race when negotiatin without peer connection
- Fix typos in log fields


## v0.0.1 (2020-03-30)

- Add RTP processing and SFU RTP logic
- Improve sender stream connectivity
- Add preliminary startup scripts and config
- Add readme and license ranger to make dist working
- Implement simple connection cleanup
- Implement SFU for KWM server RTM channels


## v0.0.0 (2020-03-18)

- Initial commit

