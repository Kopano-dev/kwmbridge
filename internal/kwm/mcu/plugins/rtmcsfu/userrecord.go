/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"time"

	"github.com/orcaman/concurrent-map"
)

type UserRecord struct {
	channel *Channel

	when time.Time
	id   string

	connections cmap.ConcurrentMap // Holds target connections for the associated user by target.
	senders     cmap.ConcurrentMap // Holds the connection which receive streams..
}

func NewUserRecord(channel *Channel, id string) *UserRecord {
	return &UserRecord{
		channel: channel,

		when: time.Now(),
		id:   id,

		connections: cmap.New(),
		senders:     cmap.New(),
	}
}

func (record *UserRecord) reset() {
	record.channel = nil

	record.connections = nil
	record.senders = nil
}
