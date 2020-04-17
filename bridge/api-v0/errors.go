/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package api

import (
	"errors"
)

const (
	ErrorCodeUnspecifiedError = "ErrorUnspecifiedError"
)

var ErrNotFound = errors.New("not found")
