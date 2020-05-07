/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 *
 * This file is based on pkg/rtc/plugins/jitterbuffer.go from https://github.com/pion/ion
 * Copyright (c) 2019
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 *
 */

package jitterbuffer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/sirupsen/logrus"
)

const (
	// bandwidth range(kbps)
	minBandwidth = 90
	maxBandwidth = 2000
)

type Config struct {
	Logger logrus.FieldLogger

	PLIInterval  int
	RembInterval int
	Bandwidth    int
}

type JitterBuffer struct {
	id string

	logger logrus.FieldLogger
	ctx    context.Context

	buffers sync.Map
	rtcpCh  chan rtcp.Packet

	config *Config
}

func New(id string, config *Config) *JitterBuffer {
	j := &JitterBuffer{
		id: id,

		logger: config.Logger,

		rtcpCh: make(chan rtcp.Packet, 100),

		config: config,
	}

	if config.Bandwidth < minBandwidth {
		config.Bandwidth = minBandwidth
	} else if config.Bandwidth > maxBandwidth {
		config.Bandwidth = maxBandwidth
	}

	return j
}

func (j *JitterBuffer) Start(ctx context.Context) error {
	if j.ctx != nil {
		return errors.New("already started")
	}

	j.ctx = ctx

	go j.startPLILoop()
	go j.startRembLoop()

	return nil
}

func (j *JitterBuffer) Stop() {
	var buffers []*Buffer
	j.buffers.Range(func(key interface{}, value interface{}) bool {
		buffers = append(buffers, value.(*Buffer))
		j.buffers.Delete(key)
		return true
	})
	for _, b := range buffers {
		b.Stop()
	}
}

func (j *JitterBuffer) startPLILoop() {
	ctx := j.ctx

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if j.config.PLIInterval <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(j.config.PLIInterval) * time.Second):
		}

		j.buffers.Range(func(key interface{}, value interface{}) bool {
			b := value.(*Buffer)
			if b.IsVideo() {
				//j.logger.WithField("id", j.id).Debugln("jjj pli loop")
				pli := &rtcp.PictureLossIndication{SenderSSRC: b.GetSSRC(), MediaSSRC: b.GetSSRC()}
				j.rtcpCh <- pli
			}
			return true
		})
	}
}

func (j *JitterBuffer) startRembLoop() {
	ctx := j.ctx

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if j.config.RembInterval <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(j.config.RembInterval) * time.Second):
		}

		var lostRate float64
		var bandwidth uint64
		j.buffers.Range(func(key interface{}, value interface{}) bool {
			b := value.(*Buffer)
			lostRate, bandwidth = b.GetLostRateBandwidth(uint64(j.config.RembInterval))
			if false {
				j.logger.WithFields(logrus.Fields{
					"jitter_id":   j.id,
					"ssrc":        b.GetSSRC(),
					"lostRate":    lostRate,
					"bandwidth":   bandwidth,
					"payloadType": b.GetPayloadType(),
					"isVideo":     b.IsVideo(),
				}).Debugln("jjj remb loop")
			}
			var bw uint64
			switch {
			case lostRate == 0 && bandwidth == 0:
				bw = uint64(j.config.Bandwidth)
			case lostRate >= 0 && lostRate < 0.1:
				bw = bandwidth * 2
			default:
				bw = uint64(float64(bandwidth) * (1 - lostRate))
			}

			if bw < minBandwidth {
				bw = minBandwidth
			}

			if bw > maxBandwidth {
				bw = maxBandwidth
			}

			remb := &rtcp.ReceiverEstimatedMaximumBitrate{
				SenderSSRC: b.GetSSRC(),
				Bitrate:    bw * 1000,
				SSRCs:      []uint32{b.GetSSRC()},
			}
			j.rtcpCh <- remb

			return true
		})
	}
}

func (j *JitterBuffer) startNackLoop(b *Buffer) {
	ctx := j.ctx

	for {
		select {
		case <-ctx.Done():
			return
		case nack := <-b.GetRTCPChan():
			//j.logger.WithField("jitter_id", j.id).Debugln("zzz nack loop nack", nack)
			if nack == nil {
				// Channel was closed.
				return
			}
			j.rtcpCh <- nack
		}
	}
}

func (j *JitterBuffer) ID() string {
	return j.id
}

func (j *JitterBuffer) GetRTCPChan() chan rtcp.Packet {
	return j.rtcpCh
}

func (j *JitterBuffer) AddBuffer(ssrc uint32, payloadType uint8, isVideo bool) *Buffer {
	b := NewBuffer(ssrc, payloadType, isVideo)
	value, loaded := j.buffers.LoadOrStore(ssrc, b)
	if loaded {
		return value.(*Buffer)
	}
	go j.startNackLoop(b)
	return b
}

func (j *JitterBuffer) GetBuffer(ssrc uint32) *Buffer {
	b, loaded := j.buffers.Load(ssrc)
	if !loaded {
		return nil
	}
	return b.(*Buffer)
}

func (j *JitterBuffer) RemoveBuffer(ssrc uint32) {
	b, loaded := j.buffers.Load(ssrc)
	if loaded {
		j.buffers.Delete(ssrc)
		b.(*Buffer).Stop()
	}
}

func (j *JitterBuffer) GetBuffers() map[uint32]*Buffer {
	buffers := make(map[uint32]*Buffer)
	j.buffers.Range(func(key interface{}, value interface{}) bool {
		buffers[key.(uint32)] = value.(*Buffer)
		return true
	})
	return buffers
}

func (j *JitterBuffer) PushRTP(pkt *rtp.Packet, isVideo bool) error {
	ssrc := pkt.SSRC

	buffer := j.GetBuffer(ssrc)
	if buffer == nil {
		buffer = j.AddBuffer(ssrc, pkt.PayloadType, isVideo)
	}

	buffer.Push(pkt)
	return nil
}

func (j *JitterBuffer) GetPacket(ssrc uint32, sn uint16) *rtp.Packet {
	b, loaded := j.buffers.Load(ssrc)
	if !loaded {
		return nil
	}
	return b.(*Buffer).GetPacket(sn)
}
