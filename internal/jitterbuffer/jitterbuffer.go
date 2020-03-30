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
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/sirupsen/logrus"
)

const (
	// bandwidth range(kbps)
	minBandwidth = 200
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

	buffers   map[uint32]*Buffer
	rtcpCh    chan rtcp.Packet
	bandwidth uint64
	lostRate  float64

	config *Config
}

func New(id string, config *Config) *JitterBuffer {
	j := &JitterBuffer{
		id: id,

		logger: config.Logger,

		buffers: make(map[uint32]*Buffer),
		rtcpCh:  make(chan rtcp.Packet, 100),

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
	for _, buffer := range j.buffers {
		buffer.Stop()
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

		for _, buffer := range j.GetBuffers() {
			if buffer.IsVideo() {
				//j.logger.WithField("id", j.id).Debugln("jjj pli loop")
				pli := &rtcp.PictureLossIndication{SenderSSRC: buffer.GetSSRC(), MediaSSRC: buffer.GetSSRC()}
				j.rtcpCh <- pli
			}
		}
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

		for _, buffer := range j.GetBuffers() {
			j.lostRate, j.bandwidth = buffer.GetLostRateBandwidth(uint64(j.config.RembInterval))
			/*j.logger.WithFields(logrus.Fields{
				"jitter_id": j.id,
				"lostRate":  j.lostRate,
				"bandwidth": j.bandwidth,
			}).Debugln("jjj remb loop")*/
			var bw uint64
			if j.lostRate == 0 && j.bandwidth == 0 {
				bw = uint64(j.config.Bandwidth)
			} else if j.lostRate >= 0 && j.lostRate < 0.1 {
				bw = uint64(j.bandwidth * 2)
			} else {
				bw = uint64(float64(j.bandwidth) * (1 - j.lostRate))
			}

			if bw < minBandwidth {
				bw = minBandwidth
			}

			if bw > uint64(j.config.Bandwidth) {
				bw = uint64(j.config.Bandwidth)
			}

			remb := &rtcp.ReceiverEstimatedMaximumBitrate{
				SenderSSRC: buffer.GetSSRC(),
				Bitrate:    bw * 1000,
				SSRCs:      []uint32{buffer.GetSSRC()},
			}
			j.rtcpCh <- remb
		}
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
	j.buffers[ssrc] = b
	go j.startNackLoop(b)
	return b
}

func (j *JitterBuffer) GetBuffer(ssrc uint32) *Buffer {
	return j.buffers[ssrc]
}

func (j *JitterBuffer) GetBuffers() map[uint32]*Buffer {
	return j.buffers
}

func (j *JitterBuffer) PushRTP(pkt *rtp.Packet, isVideo bool) error {
	ssrc := pkt.SSRC

	buffer := j.GetBuffer(ssrc)
	if buffer == nil {
		buffer = j.AddBuffer(ssrc, pkt.PayloadType, isVideo)
	}
	if buffer == nil {
		return errors.New("buffer is nil")
	}

	buffer.Push(pkt)
	return nil
}

func (j *JitterBuffer) GetPacket(ssrc uint32, sn uint16) *rtp.Packet {
	buffer := j.buffers[ssrc]
	if buffer == nil {
		return nil
	}
	return buffer.GetPacket(sn)
}
