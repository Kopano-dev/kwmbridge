/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package odata

import (
	"context"
	"net/http"
)

type key int

const (
	odataKey key = 0
)

type OData struct {
	Context string
}

func newContextWithOData(ctx context.Context, req *http.Request) context.Context {
	odata := &OData{
		Context: req.URL.Path,
	}

	return context.WithValue(ctx, odataKey, odata)
}

func FromContext(ctx context.Context) *OData {
	return ctx.Value(odataKey).(*OData)
}

func WithOData(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		ctx := newContextWithOData(req.Context(), req)
		next.ServeHTTP(rw, req.WithContext(ctx))
	})
}
