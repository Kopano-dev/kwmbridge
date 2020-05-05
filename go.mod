module stash.kopano.io/kwm/kwmbridge

go 1.14

require (
	github.com/gorilla/mux v1.7.4
	github.com/justinas/alice v1.2.0
	github.com/klauspost/compress v1.10.5 // indirect
	github.com/konsorten/go-windows-terminal-sequences v1.0.3 // indirect
	github.com/longsleep/go-metrics v0.0.0-20191013204616-cddea569b0ea
	github.com/lucas-clemente/quic-go v0.15.5 // indirect
	github.com/orcaman/concurrent-map v0.0.0-20190826125027-8c72a8bb44f6
	github.com/petermattis/goid v0.0.0-20180202154549-b0b1615b78e5 // indirect
	github.com/pion/logging v0.2.2
	github.com/pion/rtcp v1.2.1
	github.com/pion/rtp v1.4.0
	github.com/pion/webrtc/v2 v2.2.9-0.20200505120320-1ba672fd111a
	github.com/prometheus/client_golang v1.5.1
	github.com/prometheus/procfs v0.0.11 // indirect
	github.com/rogpeppe/fastuuid v1.2.0
	github.com/sasha-s/go-deadlock v0.2.0
	github.com/sirupsen/logrus v1.5.0
	github.com/spf13/cobra v0.0.7
	golang.org/x/sys v0.0.0-20200331124033-c3d80250170d // indirect
	gopkg.in/square/go-jose.v2 v2.4.1 // indirect
	nhooyr.io/websocket v1.8.5
	stash.kopano.io/kc/libkcoidc v0.8.1
	stash.kopano.io/kgol/ksurveyclient-go v0.6.1
	stash.kopano.io/kgol/oidc-go v0.3.1 // indirect
	stash.kopano.io/kgol/rndm v1.1.0
	stash.kopano.io/kwm/kwmserver v1.1.1
)

replace nhooyr.io/websocket => github.com/nhooyr/websocket v1.8.5 // Fetch directly from Github

replace github.com/marten-seemann/qtls => github.com/marten-seemann/qtls v0.9.1 // Pin version with license
