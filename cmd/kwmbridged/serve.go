/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sasha-s/go-deadlock"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"stash.kopano.io/kgol/ksurveyclient-go/autosurvey"
	"stash.kopano.io/kgol/ksurveyclient-go/prometrics"

	"stash.kopano.io/kwm/kwmbridge/bridge/server"
	cfg "stash.kopano.io/kwm/kwmbridge/config"
	"stash.kopano.io/kwm/kwmbridge/version"
)

const defaultListenAddr = "127.0.0.1:8779"

var (
	detectDeadlocks = true
)

func commandServe() *cobra.Command {
	serveCmd := &cobra.Command{
		Use:   "serve [...args]",
		Short: "Start server and listen for requests",
		Run: func(cmd *cobra.Command, args []string) {
			if err := serve(cmd, args); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}
	serveCmd.Flags().String("listen", "", fmt.Sprintf("TCP listen address (default \"%s\")", defaultListenAddr))
	serveCmd.Flags().String("iss", "", "OIDC issuer URL")
	serveCmd.Flags().Bool("insecure", false, "Disable TLS certificate and hostname validation")
	serveCmd.Flags().Bool("log-timestamp", true, "Prefix each log line with timestamp")
	serveCmd.Flags().String("log-level", "info", "Log level (one of panic, fatal, error, warn, info or debug)")
	serveCmd.Flags().Bool("with-pprof", false, "With pprof enabled")
	serveCmd.Flags().String("pprof-listen", "127.0.0.1:6060", "TCP listen address for pprof")
	serveCmd.Flags().Bool("with-metrics", false, "Enable metrics")
	serveCmd.Flags().String("metrics-listen", "127.0.0.1:6779", "TCP listen address for metrics")
	serveCmd.Flags().StringArray("kwmserver-url", []string{"http://127.0.0.1:8778/"}, "URL of kwmserver to connect, defaults to http://127.0.0.1:8778/")
	serveCmd.Flags().StringArray("use-ice-if", nil, "Interface to use when gathering ICE candidates, all interfaces will be used if not set")
	serveCmd.Flags().StringArray("use-ice-network-type", nil, "ICE network type supported when gathering candidates, if not set all types (udp4, udp6, tcp4, tcp6) are enabled")
	serveCmd.Flags().String("use-ice-udp-port-range", "", "Range of ephemeral ports that ICE UDP connections can allocate from in format min:max, if not set its not limited")
	serveCmd.Flags().BoolVar(&detectDeadlocks, "with-deadlock-detector", detectDeadlocks, "Enable deadlock detection")

	return serveCmd
}

func serve(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	logTimestamp, _ := cmd.Flags().GetBool("log-timestamp")
	logLevel, _ := cmd.Flags().GetString("log-level")

	logger, err := newLogger(!logTimestamp, logLevel)
	if err != nil {
		return fmt.Errorf("failed to create logger: %v", err)
	}
	logger.Infoln("serve start")

	deadlock.Opts.Disable = !detectDeadlocks
	deadlock.Opts.DeadlockTimeout = 15 * time.Second
	if !deadlock.Opts.Disable {
		logger.Warnln("enabled automatic deadlock detector")
	}

	config := &cfg.Config{
		Logger: logger,

		// Initialize survey client data with operational usage.
		Survey: prometrics.WrapRegistry(autosurvey.DefaultRegistry, map[string]string{
			// TODO(longsleep): Add operational data.
		}),
	}

	if issString, errIf := cmd.Flags().GetString("iss"); errIf == nil && issString != "" {
		config.Iss, errIf = url.Parse(issString)
		if errIf != nil {
			return fmt.Errorf("invalid iss url: %w", errIf)
		}
	}

	listenAddr, _ := cmd.Flags().GetString("listen")
	if listenAddr == "" {
		listenAddr = os.Getenv("KWMBRIDGED_LISTEN")
	}
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}
	config.ListenAddr = listenAddr

	if kwmServerURLStrings, _ := cmd.Flags().GetStringArray("kwmserver-url"); kwmServerURLStrings != nil {
		config.KWMServerURIs = make([]*url.URL, 0)
		for _, uriString := range kwmServerURLStrings {
			u, uriErr := url.Parse(uriString)
			if uriErr != nil {
				return fmt.Errorf("invalid kwmserver-url: %w", uriErr)
			}
			u.Path = strings.TrimRight(u.Path, "/") // Always trim trailing slash to simplify url generation later.
			config.KWMServerURIs = append(config.KWMServerURIs, u)
		}
	}
	if len(config.KWMServerURIs) == 0 {
		return fmt.Errorf("kwmserver-uri required but not given")
	}

	if ICEInterfaceStrings, _ := cmd.Flags().GetStringArray("use-ice-if"); ICEInterfaceStrings != nil {
		config.ICEInterfaces = ICEInterfaceStrings
		logger.WithField("interfaces", config.ICEInterfaces).Infoln("limiting ICE interfaces")
	}
	if ICENetworkTypeStrings, _ := cmd.Flags().GetStringArray("use-ice-network-type"); ICENetworkTypeStrings != nil {
		config.ICENetworkTypes = ICENetworkTypeStrings
		logger.WithField("types", config.ICENetworkTypes).Infoln("limiting ICE network types")
	}
	if ICEEphemeralUDPPortRangeString, _ := cmd.Flags().GetString("use-ice-udp-port-range"); ICEEphemeralUDPPortRangeString != "" {
		ICEEphemeralUDPPortRangeMinMaxStrings := strings.SplitN(ICEEphemeralUDPPortRangeString, ":", 2)
		config.ICEEphemeralUDPPortRange = [2]uint16{10000, ^uint16(0)}
		if ICEEphemeralUDPPortRangeMinMaxStrings[0] != "" {
			if minPort, portErr := strconv.ParseUint(ICEEphemeralUDPPortRangeMinMaxStrings[0], 10, 16); portErr != nil {
				return fmt.Errorf("invalid min port value in use-ice-udp-port-range: %w", portErr)
			} else {
				config.ICEEphemeralUDPPortRange[0] = uint16(minPort)
			}
		}
		if len(ICEEphemeralUDPPortRangeMinMaxStrings) > 1 && ICEEphemeralUDPPortRangeMinMaxStrings[1] != "" {
			if maxPort, portErr := strconv.ParseUint(ICEEphemeralUDPPortRangeMinMaxStrings[1], 10, 16); portErr != nil {
				return fmt.Errorf("invalid max port value in use-ice-udp-port-range: %w", portErr)
			} else {
				if maxPort <= uint64(config.ICEEphemeralUDPPortRange[0]) {
					return fmt.Errorf("max port value in use-ice-udp-port-range must be higher than min port %d", config.ICEEphemeralUDPPortRange[0])
				}
				config.ICEEphemeralUDPPortRange[1] = uint16(maxPort)
			}
		}
		logger.WithFields(logrus.Fields{
			"min": config.ICEEphemeralUDPPortRange[0],
			"max": config.ICEEphemeralUDPPortRange[1],
		}).Infoln("limiting ICE port range")
	}

	var tlsClientConfig *tls.Config
	tlsInsecureSkipVerify, _ := cmd.Flags().GetBool("insecure")
	if tlsInsecureSkipVerify {
		// NOTE(longsleep): This disable http2 client support. See https://github.com/golang/go/issues/14275 for reasons.
		tlsClientConfig = &tls.Config{
			InsecureSkipVerify: tlsInsecureSkipVerify,
		}
		logger.Warnln("insecure mode, TLS client connections are susceptible to man-in-the-middle attacks")
		logger.Debugln("http2 client support is disabled (insecure mode)")
	}
	config.HTTPClient = &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig:       tlsClientConfig,
		},
	}

	// Metrics support.
	config.WithMetrics, _ = cmd.Flags().GetBool("with-metrics")
	metricsListenAddr, _ := cmd.Flags().GetString("metrics-listen")
	if config.WithMetrics && metricsListenAddr != "" {
		reg := prometheus.NewPedanticRegistry()
		config.Metrics = prometheus.WrapRegistererWithPrefix("kwmbridged_", reg)
		// Add the standard process and Go metrics to the custom registry.
		reg.MustRegister(
			prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
			prometheus.NewGoCollector(),
		)
		go func() {
			metricsListen := metricsListenAddr
			handler := http.NewServeMux()
			logger.WithField("listenAddr", metricsListen).Infoln("metrics enabled, starting listener")
			handler.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
			err := http.ListenAndServe(metricsListen, handler)
			if err != nil {
				logger.WithError(err).Errorln("unable to start metrics listener")
			}
		}()
	}

	srv, err := server.NewServer(config)
	if err != nil {
		return fmt.Errorf("failed to create server: %v", err)
	}

	// Profiling support.
	withPprof, _ := cmd.Flags().GetBool("with-pprof")
	pprofListenAddr, _ := cmd.Flags().GetString("pprof-listen")
	if withPprof && pprofListenAddr != "" {
		runtime.SetMutexProfileFraction(5)
		go func() {
			pprofListen := pprofListenAddr
			logger.WithField("listenAddr", pprofListen).Infoln("pprof enabled, starting listener")
			err := http.ListenAndServe(pprofListen, nil)
			if err != nil {
				logger.WithError(err).Errorln("unable to start pprof listener")
			}
		}()
	}

	// Survey support.
	var guid []byte
	if config.Iss != nil && config.Iss.Hostname() != "localhost" {
		guid = []byte(config.Iss.String())
	}
	err = autosurvey.Start(ctx, "kwmbridged", version.Version, guid)
	if err != nil {
		return fmt.Errorf("failed to start auto survey: %v", err)
	}

	logger.Infoln("serve started")
	return srv.Serve(ctx)
}
