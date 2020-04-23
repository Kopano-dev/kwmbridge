/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package rtmcsfu

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/orcaman/concurrent-map"
)

type UserRecord struct {
	channel *Channel

	when   time.Time
	id     string
	closed int32

	connections cmap.ConcurrentMap // Holds target connections for the associated user by target.
	publishers  cmap.ConcurrentMap // Holds the connection which receive published streams.
}

func NewUserRecord(channel *Channel, id string) *UserRecord {
	return &UserRecord{
		channel: channel,

		when: time.Now(),
		id:   id,

		connections: cmap.New(),
		publishers:  cmap.New(),
	}
}

func (record *UserRecord) close() error {
	if closed := atomic.SwapInt32(&record.closed, 1); closed == 1 {
		// Already closed.
		return errors.New("already closed")
	}

	channel := record.channel

	record.publishers.IterCb(func(target string, record interface{}) {
		connectionRecord := record.(*ConnectionRecord)
		connectionRecord.Lock()
		connectionRecord.reset(channel.sfu.wsCtx)
		connectionRecord.Unlock()
	})

	record.connections.IterCb(func(target string, record interface{}) {
		connectionRecord := record.(*ConnectionRecord)
		connectionRecord.Lock()
		connectionRecord.reset(channel.sfu.wsCtx)
		connectionRecord.Unlock()
	})

	return nil
}

func (record *UserRecord) isClosed() bool {
	return atomic.LoadInt32(&record.closed) == 1
}
