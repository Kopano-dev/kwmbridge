/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"

	"stash.kopano.io/kwm/kwmbridge/version"
)

func commandHealthcheck() *cobra.Command {
	healthcheckCmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "KWM server health check",
		Run: func(cmd *cobra.Command, args []string) {
			if err := healthcheck(cmd, args); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}

	healthcheckCmd.Flags().String("hostname", defaultListenAddr, "Host and port where konnectd is listening")
	healthcheckCmd.Flags().String("path", "/health-check", "URL path and optional parameters to health-check endpoint")
	healthcheckCmd.Flags().String("scheme", "http", "URL scheme")
	healthcheckCmd.Flags().Bool("insecure", false, "Disable TLS certificate and hostname validation")

	return healthcheckCmd
}

func healthcheck(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	uri := url.URL{}
	uri.Scheme, _ = cmd.Flags().GetString("scheme")
	uri.Host, _ = cmd.Flags().GetString("hostname")
	uri.Path, _ = cmd.Flags().GetString("path")

	insecure, _ := cmd.Flags().GetBool("insecure")
	client := func() http.Client {
		transport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				DualStack: true,
			}).DialContext,
		}
		if insecure {
			transport.TLSClientConfig = &tls.Config{
				ClientSessionCache: tls.NewLRUClientSessionCache(0),
				InsecureSkipVerify: true,
			}
		}
		return http.Client{
			Timeout:   time.Second * 60,
			Transport: transport,
		}
	}()

	request, err := http.NewRequest(http.MethodPost, uri.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create healthcheck request: %v", err)
	}

	request.Header.Set("Connection", "close")
	request.Header.Set("User-Agent", "Kopano-Kwmbridge/"+version.Version)
	request = request.WithContext(ctx)

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("healthcheck request failed: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		bodyBytes, _ := ioutil.ReadAll(response.Body)
		fmt.Fprintf(os.Stderr, string(bodyBytes))

		return fmt.Errorf("healthcheck failed with status: %v", response.StatusCode)
	} else {
		fmt.Fprintf(os.Stdout, "healthcheck successful\n")
	}

	return nil
}
