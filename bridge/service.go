/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 */

package bridge

import (
	_ "stash.kopano.io/kwm/kwmbridge" // Import to ensure correct path.
)

// Service is an interface for services providing information about activity.
type Service interface {
	NumActive() uint64
}

// Services is a defined collection of services which handle activity.
type Services struct {
	MCUCManager Service
}

// Services returns all active services of the accociated Services as iterable.
func (services *Services) Services() []Service {
	s := make([]Service, 0)

	if services.MCUCManager != nil {
		s = append(s, services.MCUCManager)
	}

	return s
}
