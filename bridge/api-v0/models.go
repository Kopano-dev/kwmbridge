/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package api

import (
	"fmt"
)

type CollectionResource struct {
	ODataContext  string `json:"@odata.context"`
	ODataNextLink string `json:"@odata.nextLink,omitempty"`

	Values Collection `json:"values"`
}

type Collection interface{}

type ItemResource struct {
	ODataContext string `json:"@odata.context"`
	Item
}

type Item interface{}

type ErrorResource struct {
	Error interface{}
}

type ErrorWithCodeAndMessage struct {
	Code    string `json:"code"`
	Message string `json:"message"`

	innerError error
}

func NewErrorWithCodeAndMessage(code string, message string, err error) *ErrorWithCodeAndMessage {
	return &ErrorWithCodeAndMessage{
		Code:    code,
		Message: message,

		innerError: err,
	}
}

func (err *ErrorWithCodeAndMessage) Error() string {
	code := err.Code
	message := err.Message
	if message == "" && err.innerError != nil {
		message = err.innerError.Error()
	}

	return fmt.Sprintf("%s: %s", code, message)
}

func (err *ErrorWithCodeAndMessage) Unwrap() error {
	return err.innerError
}
