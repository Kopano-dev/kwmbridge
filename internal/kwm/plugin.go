/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package kwm

import (
	"context"
)

type Plugin interface {
	Start(context.Context) error
	Close() error
}
