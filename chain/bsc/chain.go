/*
 * Copyright 2020 ICON Foundation
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package bsc

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/icon-project/btp/chain"
	"github.com/icon-project/btp/common/codec"
	"github.com/icon-project/btp/common/db"
	"github.com/icon-project/btp/common/errors"
	"github.com/icon-project/btp/common/log"
	"github.com/icon-project/btp/common/mta"
)

const (
	DefaultDBDir  = "db"
	DefaultDBType = db.GoLevelDBBackend
	// DefaultBufferScaleOfBlockProof Base64 in:out = 6:8
	DefaultBufferScaleOfBlockProof  = 0.5
	DefaultBufferNumberOfBlockProof = 100
	DefaultBufferInterval           = 5 * time.Second
	DefaultReconnectDelay           = time.Second
)

type SimpleChain struct {
	s       chain.Sender
	r       chain.Receiver
	src     chain.BtpAddress
	acc     *mta.ExtAccumulator
	dst     chain.BtpAddress
	bs      *chain.BMCLinkStatus //getstatus(dst.src)
	relayCh chan *chain.RelayMessage
	l       log.Logger
	cfg     *chain.Config

	rms             []*chain.RelayMessage
	rmsMtx          sync.RWMutex
	rmSeq           uint64
	heightOfDst     int64
	lastBlockUpdate *chain.BlockUpdate
}

func (s *SimpleChain) _hasWait(rm *chain.RelayMessage) bool {
	for _, segment := range rm.Segments {
		if segment != nil && segment.GetResultParam != nil && segment.TransactionResult == nil {
			return true
		}
	}
	return false
}

func (s *SimpleChain) _log(prefix string, rm *chain.RelayMessage, segment *chain.Segment, segmentIdx int) {
	if segment == nil {
		s.l.Debugf("%s rm:%d bu:%d ~ %d rps:%d",
			prefix,
			rm.Seq,
			rm.BlockUpdates[0].Height,
			rm.BlockUpdates[len(rm.BlockUpdates)-1].Height,
			len(rm.ReceiptProofs))
	} else {
		s.l.Debugf("%s rm:%d [i:%d,h:%d,bu:%d,seq:%d,evt:%d,txh:%v]",
			prefix,
			rm.Seq,
			segmentIdx,
			segment.Height,
			segment.NumberOfBlockUpdate,
			segment.EventSequence,
			segment.NumberOfEvent,
			segment.GetResultParam)
	}
}

func (s *SimpleChain) _relay() {
	s.rmsMtx.RLock()
	defer s.rmsMtx.RUnlock()
	var err error
	for _, rm := range s.rms {
		if (len(rm.BlockUpdates) == 0 && len(rm.ReceiptProofs) == 0) || s._hasWait(rm) {
			break
		} else {
			if len(rm.Segments) == 0 {
				if rm.Segments, err = s.Segment(rm, s.bs.Verifier.Height); err != nil {
					s.l.Panicf("fail to segment err:%+v", err)
				}
			}
			//s._log("before relay", rm, nil, -1)
			reSegment := true
			for j, segment := range rm.Segments {
				if segment == nil {
					continue
				}
				reSegment = false

				if segment.GetResultParam == nil {
					segment.TransactionResult = nil
					if segment.GetResultParam, err = s.s.Relay(segment); err != nil {
						s.l.Panicf("fail to Relay err:%+v", err)
					}
					s._log("after relay", rm, segment, j)
					go s.result(rm, segment)
				}
			}
			if reSegment {
				rm.Segments = rm.Segments[:0]
			}
		}
	}
}

func (s *SimpleChain) isOverLimit(size int) bool {
	return s.s.TxSizeLimit() < size
}

func (s *SimpleChain) Segment(rm *chain.RelayMessage, height int64) ([]*chain.Segment, error) {
	segments := make([]*chain.Segment, 0)
	var err error
	msg := &RelayMessage{
		BlockUpdates:  make([][]byte, 0),
		ReceiptProofs: make([][]byte, 0),
	}
	size := 0
	//TODO rm.BlockUpdates[len(rm.BlockUpdates)-1].Height <= s.bmcStatus.Verifier.Height
	//	using only rm.BlockProof
	for _, bu := range rm.BlockUpdates {
		if bu.Height <= height {
			continue
		}
		buSize := len(bu.Proof)
		if s.isOverLimit(buSize) {
			return nil, fmt.Errorf("invalid BlockUpdate.StorageProof size")
		}
		size += buSize
		if s.isOverLimit(size) {
			segment := &chain.Segment{
				Height:              msg.GetHeight(),
				NumberOfBlockUpdate: msg.GetNumberOfBlockUpdate(),
			}
			b, err := codec.RLP.MarshalToBytes(msg)
			if err != nil {
				return nil, err
			}
			segment.TransactionParam = b

			segments = append(segments, segment)
			msg = &RelayMessage{
				BlockUpdates:  make([][]byte, 0),
				ReceiptProofs: make([][]byte, 0),
			}
			size = buSize
		}
		msg.BlockUpdates = append(msg.BlockUpdates, bu.Proof)
		msg.SetHeight(bu.Height)
		msg.SetNumberOfBlockUpdate(msg.GetNumberOfBlockUpdate() + 1)
	}

	var bp []byte
	if bp, err = codec.RLP.MarshalToBytes(rm.BlockProof); err != nil {
		return nil, err
	}
	if s.isOverLimit(len(bp)) {
		return nil, fmt.Errorf("invalid BlockProof size")
	}

	var b []byte
	for _, rp := range rm.ReceiptProofs {
		if s.isOverLimit(len(rp.Proof)) {
			return nil, fmt.Errorf("invalid ReceiptProof.Proof size")
		}
		if len(msg.BlockUpdates) == 0 {
			size += len(bp)
			msg.BlockProof = bp
			msg.SetHeight(rm.BlockProof.BlockWitness.Height)
		}
		size += len(rp.Proof)
		trp := &ReceiptProof{
			Index:       rp.Index,
			Proof:       rp.Proof,
			EventProofs: make([]*chain.EventProof, 0),
		}
		for j, ep := range rp.EventProofs {
			if s.isOverLimit(len(ep.Proof)) {
				return nil, fmt.Errorf("invalid EventProof.Proof size")
			}
			size += len(ep.Proof)
			if s.isOverLimit(size) {
				if j == 0 && len(msg.BlockUpdates) == 0 {
					return nil, fmt.Errorf("BlockProof + ReceiptProof + EventProof > limit")
				}
				//
				segment := &chain.Segment{
					Height:              msg.GetHeight(),
					NumberOfBlockUpdate: msg.GetNumberOfBlockUpdate(),
					EventSequence:       big.NewInt(msg.GetEventSequence()),
					NumberOfEvent:       msg.GetNumberOfEvent(),
				}

				b, err := codec.RLP.MarshalToBytes(msg)
				if err != nil {
					return nil, err
				}
				segment.TransactionParam = b
				segments = append(segments, segment)

				msg = &RelayMessage{
					BlockUpdates:  make([][]byte, 0),
					ReceiptProofs: make([][]byte, 0),
					BlockProof:    bp,
				}
				size = len(ep.Proof)
				size += len(rp.Proof)
				size += len(bp)

				trp = &ReceiptProof{
					Index:       rp.Index,
					Proof:       rp.Proof,
					EventProofs: make([]*chain.EventProof, 0),
				}
			}
			trp.EventProofs = append(trp.EventProofs, ep)
			msg.SetEventSequence(rp.Events[j].Sequence.Int64())
			msg.SetNumberOfEvent(msg.GetNumberOfEvent() + 1)
		}

		if b, err = codec.RLP.MarshalToBytes(trp); err != nil {
			return nil, err
		}
		msg.ReceiptProofs = append(msg.ReceiptProofs, b)
	}
	//
	segment := &chain.Segment{
		Height:              msg.GetHeight(),
		NumberOfBlockUpdate: msg.GetNumberOfBlockUpdate(),
		EventSequence:       big.NewInt(msg.GetEventSequence()),
		NumberOfEvent:       msg.GetNumberOfEvent(),
	}

	b, err = codec.RLP.MarshalToBytes(msg)
	if err != nil {
		return nil, err
	}
	segment.TransactionParam = b
	segments = append(segments, segment)
	return segments, nil
}

func (s *SimpleChain) UpdateSegment(bp *chain.BlockProof, segment *chain.Segment) error {
	//p := segment.TransactionParam.(*TransactionParam)
	cd := CallData{}
	rmp := cd.Params.(BMCRelayMethodParams)
	msg := &RelayMessage{}
	b, err := base64.URLEncoding.DecodeString(rmp.Messages)
	if _, err = codec.RLP.UnmarshalFromBytes(b, msg); err != nil {
		return err
	}
	if msg.BlockProof, err = codec.RLP.MarshalToBytes(bp); err != nil {
		return err
	}
	b, err = codec.RLP.MarshalToBytes(msg)
	if err != nil {
		return err
	}
	segment.TransactionParam = b
	return err
}

func (s *SimpleChain) result(rm *chain.RelayMessage, segment *chain.Segment) {
	var err error
	segment.TransactionResult, err = s.s.GetResult(segment.GetResultParam)
	if err != nil {
		if ec, ok := errors.CoderOf(err); ok {
			s.l.Debugf("fail to GetResult GetResultParam:%v ErrorCoder:%+v",
				segment.GetResultParam, ec)
			switch ec.ErrorCode() {
			case BMVUnknown:
				//TODO panic??
			case BMVNotVerifiable:
				segment.GetResultParam = nil
			case BMVAlreadyVerified:
				segment.GetResultParam = nil
			case BMCRevertUnauthorized:
				segment.GetResultParam = nil
			default:
				s.l.Panicf("fail to GetResult GetResultParam:%v ErrorCoder:%+v",
					segment.GetResultParam, ec)
			}
		} else {
			//TODO: commented temporarily to keep the relayer running
			//s.l.Panicf("fail to GetResult GetResultParam:%v err:%+v",
			//	segment.GetResultParam, err)
			s.l.Debugf("fail to GetResult GetResultParam:%v err:%+v", segment.GetResultParam, err)
		}
	}
}

func (s *SimpleChain) _rm() *chain.RelayMessage {
	rm := &chain.RelayMessage{
		From:         s.src,
		BlockUpdates: make([]*chain.BlockUpdate, 0),
		Seq:          s.rmSeq,
	}
	s.rms = append(s.rms, rm)
	s.rmSeq += 1
	return rm
}

func (s *SimpleChain) addRelayMessage(bu *chain.BlockUpdate, rps []*chain.ReceiptProof) {
	s.rmsMtx.Lock()
	defer s.rmsMtx.Unlock()

	if s.lastBlockUpdate != nil {
		//TODO consider remained bu when reconnect
		if s.lastBlockUpdate.Height+1 != bu.Height {
			s.l.Panicf("invalid bu")
		}
	}
	s.lastBlockUpdate = bu
	rm := s.rms[len(s.rms)-1]
	if len(rm.Segments) > 0 {
		rm = s._rm()
	}
	if len(rps) > 0 {
		rm.BlockUpdates = append(rm.BlockUpdates, bu)
		rm.ReceiptProofs = rps
		rm.HeightOfDst = s.monitorHeight()
		s.l.Debugf("addRelayMessage rms:%d bu:%d rps:%d HeightOfDst:%d", len(s.rms), bu.Height, len(rps), rm.HeightOfDst)
		rm = s._rm()
	} else {
		if bu.Height <= s.bs.Verifier.Height {
			return
		}
		rm.BlockUpdates = append(rm.BlockUpdates, bu)
		s.l.Debugf("addRelayMessage rms:%d bu:%d ~ %d", len(s.rms), rm.BlockUpdates[0].Height, bu.Height)
	}
}

func (s *SimpleChain) updateRelayMessage(h int64, seq int64) (err error) {
	s.rmsMtx.Lock()
	defer s.rmsMtx.Unlock()

	s.l.Debugf("updateRelayMessage h:%d seq:%d monitorHeight:%d", h, seq, s.monitorHeight())

	rrm := 0
rmLoop:
	for i, rm := range s.rms {
		if len(rm.ReceiptProofs) > 0 {
			rrp := 0
		rpLoop:
			for j, rp := range rm.ReceiptProofs {
				revt := seq - rp.Events[0].Sequence.Int64() + 1
				if revt < 1 {
					break rpLoop
				}
				if revt >= int64(len(rp.Events)) {
					rrp = j + 1
				} else {
					s.l.Debugf("updateRelayMessage rm:%d bu:%d rp:%d removeEventProofs %d ~ %d",
						rm.Seq,
						rm.BlockUpdates[len(rm.BlockUpdates)-1].Height,
						rp.Index,
						rp.Events[0].Sequence,
						rp.Events[revt-1].Sequence)
					rp.Events = rp.Events[revt:]
					if len(rp.EventProofs) > 0 {
						rp.EventProofs = rp.EventProofs[revt:]
					}
				}
			}
			if rrp > 0 {
				s.l.Debugf("updateRelayMessage rm:%d bu:%d removeReceiptProofs %d ~ %d",
					rm.Seq,
					rm.BlockUpdates[len(rm.BlockUpdates)-1].Height,
					rm.ReceiptProofs[0].Index,
					rm.ReceiptProofs[rrp-1].Index)
				rm.ReceiptProofs = rm.ReceiptProofs[rrp:]
			}
		}
		if rm.BlockProof != nil {
			if len(rm.ReceiptProofs) > 0 {
				if rm.BlockProof, err = s.newBlockProof(rm.BlockProof.BlockWitness.Height, rm.BlockProof.Header); err != nil {
					return
				}
			} else {
				rrm = i + 1
			}
		}
		if len(rm.BlockUpdates) > 0 {
			rbu := h - rm.BlockUpdates[0].Height + 1
			if rbu < 1 {
				break rmLoop
			}
			if rbu >= int64(len(rm.BlockUpdates)) {
				if len(rm.ReceiptProofs) > 0 {
					lbu := rm.BlockUpdates[len(rm.BlockUpdates)-1]
					if rm.BlockProof, err = s.newBlockProof(lbu.Height, lbu.Header); err != nil {
						return
					}
					rm.BlockUpdates = rm.BlockUpdates[:0]
				} else {
					rrm = i + 1
				}
			} else {
				s.l.Debugf("updateRelayMessage rm:%d removeBlockUpdates %d ~ %d",
					rm.Seq,
					rm.BlockUpdates[0].Height,
					rm.BlockUpdates[rbu-1].Height)
				rm.BlockUpdates = rm.BlockUpdates[rbu:]
			}
		}
	}
	if rrm > 0 {
		s.l.Debugf("updateRelayMessage rms:%d removeRelayMessage %d ~ %d",
			len(s.rms),
			s.rms[0].Seq,
			s.rms[rrm-1].Seq)
		s.rms = s.rms[rrm:]
		if len(s.rms) == 0 {
			s._rm()
		}
	}
	return nil
}

func (s *SimpleChain) updateMTA(bu *chain.BlockUpdate) {
	next := s.acc.Height() + 1
	if next < bu.Height {
		s.l.Panicf("found missing block next:%d bu:%d", next, bu.Height)
	}
	if next == bu.Height {
		s.acc.AddHash(bu.BlockHash)
		if err := s.acc.Flush(); err != nil {
			//TODO MTA Flush error handling
			s.l.Panicf("fail to MTA Flush err:%+v", err)
		}
		//s.l.Debugf("updateMTA %d", bu.Height)
	}
}

func (s *SimpleChain) OnBlockOfDst(height int64) error {
	s.l.Tracef("OnBlockOfDst height:%d", height)
	atomic.StoreInt64(&s.heightOfDst, height)
	h, seq := s.bs.Verifier.Height, s.bs.RxSeq
	if err := s.RefreshStatus(); err != nil {
		return err
	}
	if h != s.bs.Verifier.Height || seq != s.bs.RxSeq {
		h, seq = s.bs.Verifier.Height, s.bs.RxSeq
		if err := s.updateRelayMessage(h, seq.Int64()); err != nil {
			return err
		}
		s.relayCh <- nil
	}
	return nil
}

func (s *SimpleChain) OnBlockOfSrc(bu *chain.BlockUpdate, rps []*chain.ReceiptProof) {
	s.l.Tracef("OnBlockOfSrc height:%d, bu.Height:%d", s.acc.Height(), bu.Height)
	s.updateMTA(bu)
	s.addRelayMessage(bu, rps)
	s.relayCh <- nil
}

func (s *SimpleChain) newBlockProof(height int64, header []byte) (*chain.BlockProof, error) {
	//at := s.bs.Verifier.Height
	//w, err := s.acc.WitnessForWithAccLength(height-s.acc.Offset(), at-s.bs.Verifier.Offset)
	//TODO refactoring Duplicate rlp decode
	vs := &VerifierStatus_v1{}
	_, err := codec.RLP.UnmarshalFromBytes(s.bs.Verifier.Extra, vs)
	if err != nil {
		return nil, err
	}

	at, w, err := s.acc.WitnessForAt(height, s.bs.Verifier.Height, vs.Offset)
	if err != nil {
		return nil, err
	}

	s.l.Debugf("newBlockProof height:%d, at:%d, w:%d", height, at, len(w))
	bp := &chain.BlockProof{
		Header: header,
		BlockWitness: &chain.BlockWitness{
			Height:  at,
			Witness: mta.WitnessesToHashes(w),
		},
	}
	dumpBlockProof(s.acc, height, bp)
	return bp, nil
}

func (s *SimpleChain) prepareDatabase(offset int64) error {
	s.l.Debugln("open database", filepath.Join(s.cfg.AbsBaseDir(), s.cfg.Dst.Address.NetworkAddress()))
	database, err := db.Open(s.cfg.AbsBaseDir(), string(DefaultDBType), s.cfg.Dst.Address.NetworkAddress())
	if err != nil {
		return errors.Wrap(err, "fail to open database")
	}
	defer func() {
		if err != nil {
			database.Close()
		}
	}()
	var bk db.Bucket
	if bk, err = database.GetBucket("Accumulator"); err != nil {
		return err
	}
	k := []byte("Accumulator")
	if offset < 0 {
		offset = 0
	}
	s.acc = mta.NewExtAccumulator(k, bk, offset)
	if bk.Has(k) {
		//offset will be ignore
		if err = s.acc.Recover(); err != nil {
			return errors.Wrapf(err, "fail to acc.Recover cause:%v", err)
		}
		s.l.Debugf("recover Accumulator offset:%d, height:%d", s.acc.Offset(), s.acc.Height())

		//TODO [TBD] sync offset
		//if s.acc.Offset() > offset {
		//	hashes := make([][]byte, s.acc.Offset() - offset)
		//	for i := 0; i < len(hashes); i++ {
		//		hashes[i] = getBlockHashByHeight(offset)
		//		offset++
		//	}
		//	s.acc.AddHashesToHead(hashes)
		//} else if s.acc.Offset() < offset {
		//	s.acc.RemoveHashesFromHead(offset-s.acc.Offset())
		//}
	}
	return nil
}

func (s *SimpleChain) RefreshStatus() error {
	bmcStatus, err := s.s.GetStatus()
	if err != nil {
		return err
	}
	s.bs = bmcStatus
	return nil
}

func (s *SimpleChain) init() error {
	if err := s.RefreshStatus(); err != nil {
		return err
	}
	atomic.StoreInt64(&s.heightOfDst, s.bs.CurrentHeight)
	if s.relayCh == nil {
		s.relayCh = make(chan *chain.RelayMessage, 2)
		go func() {
			s.l.Debugln("start relayLoop")
			defer func() {
				s.l.Debugln("stop relayLoop")
			}()
			for {
				select {
				case _, ok := <-s.relayCh:
					if !ok {
						return
					}
					s._relay()
				}
			}
		}()
	}
	s.l.Debugf("_init height:%d, dst(%s, height:%d, seq:%d, last:%d), receive:%d",
		s.acc.Height(), s.dst, s.bs.Verifier.Height, s.bs.RxSeq, s.bs.Verifier.Height, s.receiveHeight())
	return nil
}

func (s *SimpleChain) receiveHeight() int64 {
	//min(max(s.acc.Height(), s.bs.Verifier.Offset), s.bs.Verifier.LastHeight)
	//TODO refactoring Duplicate rlp decode
	vs := &VerifierStatus_v1{}
	_, err := codec.RLP.UnmarshalFromBytes(s.bs.Verifier.Extra, vs)
	if err != nil {
		return 0
	}

	max := s.acc.Height()
	if max < vs.Offset {
		max = vs.Offset
	}
	max += 1
	min := vs.LastHeight
	if max < min {
		min = max
	}
	return min
}

func (s *SimpleChain) monitorHeight() int64 {
	return atomic.LoadInt64(&s.heightOfDst)
}

func (s *SimpleChain) Serve(sender chain.Sender) error {
	s.s = sender
	s.r = NewReceiver(s.src, s.dst, s.cfg.Src.Endpoint, s.cfg.Src.Options, s.l)

	if err := s.prepareDatabase(s.cfg.Offset); err != nil {
		return err
	}

	if err := s.Monitoring(); err != nil {
		return err
	}

	return nil
}

func (s *SimpleChain) Monitoring() error {
	if err := s.init(); err != nil {
		return err
	}
	errCh := make(chan error)
	go func() {
		err := s.s.MonitorLoop(
			s.bs.CurrentHeight,
			s.OnBlockOfDst,
			func() {
				s.l.Debugf("Connect MonitorLoop")
				errCh <- nil
			})
		select {
		case errCh <- err:
		default:
		}
	}()
	go func() {
		err := s.r.ReceiveLoop(
			s.receiveHeight(),
			s.bs.RxSeq,
			s.OnBlockOfSrc,
			func() {
				s.l.Debugf("Connect ReceiveLoop")
				errCh <- nil
			})
		select {
		case errCh <- err:
		default:
		}
	}()
	for {
		select {
		case err := <-errCh:
			if err != nil {
				return err
			}
		}
	}
}

func NewChain(cfg *chain.Config, l log.Logger) *SimpleChain {
	s := &SimpleChain{
		src: cfg.Src.Address,
		dst: cfg.Dst.Address,
		l:   l.WithFields(log.Fields{log.FieldKeyChain:
		//fmt.Sprintf("%s->%s", cfg.Src.Address.NetworkAddress(), cfg.Dst.Address.NetworkAddress())}),
		fmt.Sprintf("%s", cfg.Dst.Address.NetworkID())}),
		cfg: cfg,
		rms: make([]*chain.RelayMessage, 0),
	}
	s._rm()
	return s
}

func dumpBlockProof(acc *mta.ExtAccumulator, height int64, bp *chain.BlockProof) {
	if n, err := acc.GetNode(height); err != nil {
		fmt.Printf("height:%d, accLen:%d, err:%+v", height, acc.Len(), err)
	} else {
		fmt.Printf("height:%d, accLen:%d, hash:%s\n", height, acc.Len(), hex.EncodeToString(n.Hash()))
	}

	fmt.Print("dumpBlockProof.height:", bp.BlockWitness.Height, ",witness:[")
	for _, w := range bp.BlockWitness.Witness {
		fmt.Print(hex.EncodeToString(w), ",")
	}
	fmt.Println("]")
	b, _ := codec.RLP.MarshalToBytes(bp)
	fmt.Println(base64.URLEncoding.EncodeToString(b))
}
