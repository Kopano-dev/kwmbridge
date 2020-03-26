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
