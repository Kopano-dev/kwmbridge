/*
 * SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright 2020 Kopano and its licensors
 *
 * This file is based pkg/rtc/plugins/buffer.go of https://github.com/pion/ion
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
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

const (
	maxSN      = 65536
	maxPktSize = 1000

	// VP8 VP9 H264 clock rate 90000Hz
	videoClock = 90000
	// buffer time 2s
	maxBufferTSDelta = videoClock * 2

	// 1+16(FSN+BLP) https://tools.ietf.org/html/rfc2032#page-9
	maxNackLostSize = 17
)

func tsDelta(x, y uint32) uint32 {
	if x > y {
		return x - y
	}
	return y - x
}

type Buffer struct {
	pktBuffer   [maxSN]*rtp.Packet
	lastNackSN  uint16
	lastClearTS uint32
	lastClearSN uint16

	// Last seqnum that has been added to buffer.
	lastPushSN uint16

	ssrc        uint32
	payloadType uint8
	video       bool

	receivedPkt int
	lostPkt     int

	rtcpCh chan rtcp.Packet

	totalByte uint64
}

func NewBuffer(ssrc uint32, payloadType uint8, isVideo bool) *Buffer {
	b := &Buffer{
		ssrc:        ssrc,
		payloadType: payloadType,
		video:       isVideo,

		rtcpCh: make(chan rtcp.Packet, maxPktSize),
	}
	return b
}

func (b *Buffer) GetRTCPChan() chan rtcp.Packet {
	return b.rtcpCh
}

func (b *Buffer) GetPayloadType() uint8 {
	return b.payloadType
}

func (b *Buffer) GetSSRC() uint32 {
	return b.ssrc
}

func (b *Buffer) IsVideo() bool {
	return b.video
}

func (b *Buffer) Push(p *rtp.Packet) {
	b.receivedPkt++
	b.totalByte += uint64(p.MarshalSize())

	// Init ssrc payloadType.
	if b.ssrc == 0 || b.payloadType == 0 {
		b.ssrc = p.SSRC
		b.payloadType = p.PayloadType
	}

	// Init lastClearTS.
	if b.lastClearTS == 0 {
		b.lastClearTS = p.Timestamp
	}

	// Init lastClearSN.
	if b.lastClearSN == 0 {
		b.lastClearSN = p.SequenceNumber
	}

	// Init lastNackSN.
	if b.lastNackSN == 0 {
		b.lastNackSN = p.SequenceNumber
	}

	b.pktBuffer[p.SequenceNumber] = p
	b.lastPushSN = p.SequenceNumber

	// Clear old packet by timestamp.
	b.clearOldPkt(p.Timestamp, p.SequenceNumber)

	// Limit nack range.
	if b.lastPushSN-b.lastNackSN >= maxNackLostSize {
		b.lastNackSN = b.lastPushSN - maxNackLostSize
	}

	if b.lastPushSN-b.lastNackSN >= maxNackLostSize {
		// Calc [lastNackSN, lastpush-8] if has keyframe.
		nackPair, lostPkt := b.GetNackPair(b.pktBuffer, b.lastNackSN, b.lastPushSN)
		b.lastNackSN = b.lastPushSN
		if lostPkt > 0 {
			b.lostPkt += lostPkt
			nack := &rtcp.TransportLayerNack{
				// Origin ssrc.
				SenderSSRC: b.ssrc,
				MediaSSRC:  b.ssrc,
				Nacks: []rtcp.NackPair{
					nackPair,
				},
			}
			b.rtcpCh <- nack
		}
	}
}

func (b *Buffer) clearOldPkt(pushPktTS uint32, pushPktSN uint16) {
	clearTS := b.lastClearTS
	clearSN := b.lastClearSN
	if tsDelta(pushPktTS, clearTS) >= maxBufferTSDelta {
		for i := clearSN + 1; i <= pushPktSN; i++ {
			if b.pktBuffer[i] == nil {
				continue
			}
			if tsDelta(pushPktTS, b.pktBuffer[i].Timestamp) >= maxBufferTSDelta {
				b.lastClearTS = b.pktBuffer[i].Timestamp
				b.lastClearSN = i
				b.pktBuffer[i] = nil
			} else {
				break
			}
		}
	}
}

func (b *Buffer) GetNackPair(buffer [maxSN]*rtp.Packet, begin, end uint16) (rtcp.NackPair, int) {
	var lostPkt int

	// Size is <= 17.
	if end-begin > maxNackLostSize {
		return rtcp.NackPair{}, lostPkt
	}

	// Bitmask of following lost packets (BLP).
	blp := uint16(0)
	lost := uint16(0)

	// Find first lost pkt.
	for i := begin; i < end; i++ {
		if buffer[i] == nil {
			lost = i
			lostPkt++
			break
		}
	}

	// No packet lost.
	if lost == 0 {
		return rtcp.NackPair{}, lostPkt
	}

	// Calc blp.
	for i := lost; i < end; i++ {
		// Calc from next lost packet.
		if i > lost && buffer[i] == nil {
			blp = blp | (1 << (i - lost - 1))
			lostPkt++
		}
	}

	return rtcp.NackPair{PacketID: lost, LostPackets: rtcp.PacketBitmap(blp)}, lostPkt
}

func (b *Buffer) GetLostRateBandwidth(cycle uint64) (float64, uint64) {
	lostRate := float64(b.lostPkt) / float64(b.receivedPkt+b.lostPkt)
	byteRate := b.totalByte / cycle
	b.receivedPkt, b.lostPkt, b.totalByte = 0, 0, 0
	return lostRate, byteRate * 8 / 1000
}
