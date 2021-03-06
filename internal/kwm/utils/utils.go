/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package utils

import (
	"crypto/rand"
	"encoding/base64"
	"math/big"
	"net/url"

	"github.com/rogpeppe/fastuuid"
	"stash.kopano.io/kgol/rndm"
)

var guidGenerator = fastuuid.MustNewGenerator()

const maxUint32 = ^uint32(0)

func AsWebsocketURL(uriString string) (string, error) {
	uri, err := url.Parse(uriString)
	if err != nil {
		return "", err
	}

	switch uri.Scheme {
	case "https":
		uri.Scheme = "wss"
	case "http":
		uri.Scheme = "ws"
	}

	return uri.String(), nil
}

func ComputeInitiator(source, target string) bool {
	if source == "" {
		return false
	}

	// NOTE(longsleep): This is opposite from the client library.
	return source < target
}

func NewRandomGUID() string {
	return guidGenerator.Hex128()
}

func NewRandomUint32() uint32 {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxUint32)))
	if err != nil {
		panic(err)
	}

	return uint32(n.Uint64())
}

func NewRandomString(n int) string {
	return base64.RawURLEncoding.EncodeToString(rndm.GenerateRandomBytes(base64.RawURLEncoding.DecodedLen(n)))
}
