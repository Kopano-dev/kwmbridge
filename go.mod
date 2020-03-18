module stash.kopano.io/kwm/kwmbridge

go 1.14

require (
	github.com/gorilla/mux v1.7.3
	github.com/longsleep/go-metrics v0.0.0-20181008184033-900f92a5ad22
	github.com/orcaman/concurrent-map v0.0.0-20190826125027-8c72a8bb44f6
	github.com/pion/logging v0.2.2
	github.com/pion/rtcp v1.2.1
	github.com/pion/webrtc/v2 v2.2.3
	github.com/prometheus/client_golang v1.5.0
	github.com/rogpeppe/fastuuid v1.2.0
	github.com/sirupsen/logrus v1.4.2
	github.com/spf13/cobra v0.0.6
	nhooyr.io/websocket v0.0.0-00010101000000-000000000000
	stash.kopano.io/kc/libkcoidc v0.7.2
	stash.kopano.io/kgol/ksurveyclient-go v0.6.1
	stash.kopano.io/kgol/rndm v1.0.0
	stash.kopano.io/kwm/kwmserver v1.1.0
)

replace nhooyr.io/websocket => github.com/nhooyr/websocket v1.8.4 // Fetch directly from Github

replace github.com/marten-seemann/qtls => github.com/marten-seemann/qtls v0.4.1 // Pin version with license
