/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package mcu

import (
	"sync"
	"time"

	"stash.kopano.io/kwm/kwmbridge/internal/kwm"
)

type AttachedRecord struct {
	sync.Mutex
	when    time.Time
	plugin  Plugin
	message *kwm.MCUTypeContainer
}
