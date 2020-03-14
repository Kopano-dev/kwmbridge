/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2018 Kopano and its licensors
 */

package bpool

import (
	"bytes"
	"sync"
)

var bpool sync.Pool

// Get returns a buffer from the pool creating a new one if the pool is empty.
func Get() *bytes.Buffer {
	b, ok := bpool.Get().(*bytes.Buffer)
	if !ok {
		b = &bytes.Buffer{}
	}
	return b
}

// Put returns the provided buffer into the pool.
func Put(b *bytes.Buffer) {
	b.Reset()
	bpool.Put(b)
}
