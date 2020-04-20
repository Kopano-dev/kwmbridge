/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"

	"stash.kopano.io/kwm/kwmbridge/bridge/odata"
)

type CollectionResource struct {
	ODataContext  string `json:"@odata.context"`
	ODataNextLink string `json:"@odata.nextLink,omitempty"`

	Values Collection `json:"values"`
}

func NewCollectionResource(values Collection, req *http.Request, nextLink *string) *CollectionResource {
	resource := &CollectionResource{
		Values: values,
	}

	if req != nil {
		od := odata.FromContext(req.Context())
		if od != nil {
			resource.ODataContext = od.Context
		} else {
			resource.ODataContext = req.URL.Path
		}
	}
	if nextLink != nil {
		resource.ODataNextLink = *nextLink
	}

	return resource
}

type Collection interface{}

type ItemResource struct {
	ODataContext string `json:"@odata.context"`
	Item
}

func NewItemResource(item Item, req *http.Request) *ItemResource {
	resource := &ItemResource{
		Item: item,
	}
	if req != nil {
		od := odata.FromContext(req.Context())
		if od != nil {
			resource.ODataContext = od.Context
		} else {
			resource.ODataContext = req.URL.Path
		}
	}

	return resource
}

func (ir ItemResource) MarshalJSON() ([]byte, error) {
	out := make(map[string]interface{})
	itemValue := reflect.Indirect(reflect.ValueOf(ir.Item))
	itemType := itemValue.Type()
	for i := 0; i < itemValue.NumField(); i++ {
		field := itemType.Field(i)
		name := field.Name
		if field.PkgPath == "" {
			out[field.Tag.Get("json")] = itemValue.FieldByName(name).Interface()
		}
	}
	resourceValue := reflect.Indirect(reflect.ValueOf(ir))
	resourceType := resourceValue.Type()
	for i := 0; i < resourceValue.NumField(); i++ {
		field := resourceType.Field(i)
		name := field.Name
		if name != "Item" {
			out[field.Tag.Get("json")] = resourceValue.FieldByName(name).Interface()
		}
	}
	return json.Marshal(out)
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
