/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package mcu

import (
	"net/http"
	"time"
)

type ClientResource struct {
	client *Client

	ID            string `json:"id"`
	URI           string `json:"uri"`
	Connected     bool   `json:"connected"`
	AttachedCount int    `json:"attached_count"`
}

func (resource *ClientResource) Attached(id string) []*AttachedResource {
	attached := make([]*AttachedResource, 0)

	if id == "" {
		resource.client.attached.IterCb(func(key string, v interface{}) {
			record := v.(*AttachedRecord)
			attached = append(attached, NewAttachedResource(record))
		})
	} else {
		if v, ok := resource.client.attached.Get(id); ok {
			record := v.(*AttachedRecord)
			attached = append(attached, NewAttachedResource(record))
		}
	}

	return attached
}

type AttachedResource struct {
	record *AttachedRecord

	ID      string      `json:"id"`
	When    time.Time   `json:"when"`
	Bridge  string      `json:"bridge"`
	Plugin  string      `json:"plugin"`
	Handle  int64       `json:"handle_id"`
	Summary interface{} `json:"summary,omitempty"`
}

func NewAttachedResource(record *AttachedRecord) *AttachedResource {
	return &AttachedResource{
		record: record,

		ID:      record.message.Transaction,
		When:    record.when,
		Bridge:  record.plugin.Bridge(),
		Plugin:  record.message.Plugin,
		Handle:  record.message.Handle,
		Summary: record.plugin.Summary(),
	}
}

func (resource *AttachedResource) BridgeHTTPHandler() http.Handler {
	if handler, ok := resource.record.plugin.(http.Handler); ok {
		return handler
	}
	return nil
}
