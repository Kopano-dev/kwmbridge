/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"
)

func WriteResourceAsJSON(rw http.ResponseWriter, resource interface{}) error {
	rw.Header().Add("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(rw)
	encoder.SetIndent("", "  ")
	return encoder.Encode(resource)
}

func WriteErrorAsJSON(rw http.ResponseWriter, err error) error {
	rw.Header().Add("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(rw)
	encoder.SetIndent("", "  ")

	switch {
	case errors.Is(err, ErrNotFound):
		rw.WriteHeader(http.StatusNotFound)
	case err == nil:
		panic("writing nil error")
	default:
		rw.WriteHeader(http.StatusInternalServerError)
	}

	var e *ErrorWithCodeAndMessage
	if !errors.As(err, &e) {
		e = NewErrorWithCodeAndMessage(ErrorCodeUnspecifiedError, fmt.Errorf("unspecified error: %w", err).Error(), nil)
	}
	return encoder.Encode(e)
}

func GetRequestVars(req *http.Request) map[string]string {
	return mux.Vars(req)
}

func GetRequestVar(req *http.Request, name string) (string, bool) {
	value, found := GetRequestVars(req)[name]
	return value, found
}

func NewErrorResource(err error) *ErrorResource {
	return &ErrorResource{
		Error: err,
	}
}
