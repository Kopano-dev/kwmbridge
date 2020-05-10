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

	go func() {
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

		logger := record.channel.logger.WithField("target", record.id)

		for item := range channel.connections.IterBuffered() {
			target := item.Key
			connectionRecord := item.Val

			logger.Debugln("xxx sfu user close", record.id, target)
			if target == record.id {
				// Do not unpublish for self.
				continue
			}
			targetRecord := connectionRecord.(*UserRecord)
			if targetRecord.isClosed() {
				// Do not unpublish for closed.
				continue
			}

			logger.Debugln("xxx sfu user close target", record.id, target)

			if removed := targetRecord.connections.RemoveCb(record.id, func(key string, r interface{}, exists bool) bool {
				if exists {
					cr := r.(*ConnectionRecord)
					cr.RLock()
					remove := cr.bound == record || cr.bound == nil
					defer func() {
						if remove {
							cr.reset(channel.sfu.wsCtx)
						}
						cr.RUnlock()
					}()
					logger.Debugln("xxx sfu user close target connection", record.id, target, remove, cr.bound == record, cr.bound == nil)
					return remove
				}
				return false
			}); removed {
				logger.Debugln("xxx sfu user close removed connection", record.id, target)
			} else {
				logger.Debugln("xxx sfu onReset not removed connection", record.id, target)
			}
		}
	}()

	return nil
}

func (record *UserRecord) isClosed() bool {
	return record == nil || atomic.LoadInt32(&record.closed) == 1
}
