/*
 * Copyright 2021 ICON Foundation
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

package icon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"github.com/icon-project/btp/chain"
	"github.com/icon-project/btp/common"
	"github.com/icon-project/btp/common/jsonrpc"
	"github.com/icon-project/btp/common/log"
)

const (
	txMaxDataSize                 = 524288 //512 * 1024 // 512kB
	txOverheadScale               = 0.37   //base64 encoding overhead 0.36, rlp and other fields 0.01
	DefaultGetRelayResultInterval = time.Second
	DefaultRelayReSendInterval    = time.Second
	DefaultStepLimit              = 0x9502f900 //maxStepLimit(invoke), refer https://www.icondev.io/docs/step-estimation
)

var (
	txSizeLimit = int(math.Ceil(txMaxDataSize / (1 + txOverheadScale)))
)

type sender struct {
	c   *Client
	src chain.BtpAddress
	dst chain.BtpAddress
	w   Wallet
	l   log.Logger
	opt struct {
		StepLimit int64
	}
	isFoundOffsetBySeq bool
	cb                 chain.ReceiveCallback
}

func (s *sender) newTransactionParam(method string, params interface{}) *TransactionParam {
	p := &TransactionParam{
		Version:     NewHexInt(JsonrpcApiVersion),
		FromAddress: Address(s.w.Address()),
		ToAddress:   Address(s.dst.Account()),
		NetworkID:   HexInt(s.dst.NetworkID()),
		StepLimit:   NewHexInt(s.opt.StepLimit),
		DataType:    "call",
		Data: &CallData{
			Method: method,
			Params: params,
		},
	}
	return p
}

type transactionParamMessage struct {
	messages string
}

func (s *sender) sendFragment(msg []byte, idx int) (chain.GetResultParam, error) {
	fmp := &BMCFragmentMethodParams{
		Prev:     s.src.String(),
		Messages: base64.URLEncoding.EncodeToString(msg),
		Index:    NewHexInt(int64(idx)),
	}
	p := s.newTransactionParam(BMCFragmentMethod, fmp)
	return s.sendTransaction(p)
}

func (s *sender) Relay(segment *chain.Segment) (chain.GetResultParam, error) {
	msg := segment.TransactionParam.([]byte)
	idx := len(msg) / txSizeLimit
	if idx == 0 {
		rmp := &BMCRelayMethodParams{
			Prev:     s.src.String(),
			Messages: base64.URLEncoding.EncodeToString(msg),
		}
		return s.sendTransaction(s.newTransactionParam(BMCRelayMethod, rmp))
	} else {
		ret, err := s.sendFragment(msg[:txSizeLimit], idx*-1)
		if err != nil {
			return nil, err
		}
		msg = msg[txSizeLimit:]
		for idx--; idx > 0; idx-- {
			if ret, err = s.sendFragment(msg[:txSizeLimit], idx); err != nil {
				return ret, err
			}
			msg = msg[txSizeLimit:]
		}
		if ret, err = s.sendFragment(msg[:], idx); err != nil {
			return ret, err
		}
		return ret, err
	}
}

func (s *sender) sendTransaction(p *TransactionParam) (chain.GetResultParam, error) {
	thp := &TransactionHashParam{}
SignLoop:
	for {
		if err := s.c.SignTransaction(s.w, p); err != nil {
			return nil, err
		}
	SendLoop:
		for {
			txh, err := s.c.SendTransaction(p)
			if txh != nil {
				thp.Hash = *txh
			}
			if err != nil {
				if je, ok := err.(*jsonrpc.Error); ok {
					switch je.Code {
					case JsonrpcErrorCodeTxPoolOverflow:
						<-time.After(DefaultRelayReSendInterval)
						continue SendLoop
					case JsonrpcErrorCodeSystem:
						if subEc, err := strconv.ParseInt(je.Message[1:5], 0, 32); err == nil {
							switch subEc {
							case DuplicateTransactionError:
								s.l.Debugf("DuplicateTransactionError txh:%v", txh)
								return thp, nil
							case ExpiredTransactionError:
								continue SignLoop
							}
						}
					}
				}
				return nil, mapError(err)
			}
			return thp, nil
		}
	}
}

func (s *sender) GetResult(p chain.GetResultParam) (chain.TransactionResult, error) {
	if txh, ok := p.(*TransactionHashParam); ok {
		for {
			txr, err := s.c.GetTransactionResult(txh)
			if err != nil {
				if je, ok := err.(*jsonrpc.Error); ok {
					switch je.Code {
					case JsonrpcErrorCodePending, JsonrpcErrorCodeExecuting:
						<-time.After(DefaultGetRelayResultInterval)
						continue
					}
				}
			}
			return txr, mapErrorWithTransactionResult(txr, err)
		}
	} else {
		return nil, fmt.Errorf("fail to cast *TransactionHashParam %T", p)
	}
}

func (s *sender) GetStatus() (*chain.BMCLinkStatus, error) {
	p := &CallParam{
		FromAddress: Address(s.w.Address()),
		ToAddress:   Address(s.dst.Account()),
		DataType:    "call",
		Data: CallData{
			Method: BMCGetStatusMethod,
			Params: BMCStatusParams{
				Target: s.src.String(),
			},
		},
	}
	bs := &BMCStatus{}
	err := mapError(s.c.Call(p, bs))
	if err != nil {
		return nil, err
	}
	ls := &chain.BMCLinkStatus{}
	if ls.TxSeq, err = bs.TxSeq.BigInt(); err != nil {
		return nil, err
	}
	if ls.RxSeq, err = bs.RxSeq.BigInt(); err != nil {
		return nil, err
	}
	if ls.Verifier.Height, err = bs.Verifier.Height.Value(); err != nil {
		return nil, err
	}
	if ls.Verifier.Extra, err = bs.Verifier.Extra.Value(); err != nil {
		return nil, err
	}
	if ls.CurrentHeight, err = bs.CurrentHeight.Value(); err != nil {
		return nil, err
	}
	return ls, nil
}

func (s *sender) MonitorLoop(height int64, cb chain.MonitorCallback, scb func()) error {
	br := &BlockRequest{
		Height: NewHexInt(height),
	}
	return s.c.MonitorBlock(br,
		func(conn *websocket.Conn, v *BlockNotification) error {
			if h, err := v.Height.Value(); err != nil {
				return err
			} else {
				return cb(h)
			}
		},
		func(conn *websocket.Conn) {
			s.l.Debugf("MonitorLoop connected %s", conn.LocalAddr().String())
			if scb != nil {
				scb()
			}
		},
		func(conn *websocket.Conn, err error) {
			s.l.Debugf("onError %s err:%+v", conn.LocalAddr().String(), err)
			_ = conn.Close()
		})
}

func (s *sender) StopMonitorLoop() {
	s.c.CloseAllMonitor()
}
func (s *sender) FinalizeLatency() int {
	//on-the-next
	return 1
}

func (s *sender) TxSizeLimit() int {
	return txSizeLimit
}

func NewSender(src, dst chain.BtpAddress, w Wallet, endpoint string, opt map[string]interface{}, l log.Logger) chain.Sender {
	s := &sender{
		src: src,
		dst: dst,
		w:   w,
		l:   l,
	}
	b, err := json.Marshal(opt)
	if err != nil {
		l.Panicf("fail to marshal opt:%#v err:%+v", opt, err)
	}
	if err = json.Unmarshal(b, &s.opt); err != nil {
		l.Panicf("fail to unmarshal opt:%#v err:%+v", opt, err)
	}
	if s.opt.StepLimit <= 0 {
		s.opt.StepLimit = DefaultStepLimit
	}
	s.c = NewClient(endpoint, l)
	return s
}

func mapError(err error) error {
	if err != nil {
		switch re := err.(type) {
		case *jsonrpc.Error:
			//fmt.Printf("jrResp.Error:%+v", re)
			switch re.Code {
			case JsonrpcErrorCodeTxPoolOverflow:
				return ErrSendFailByOverflow
			case JsonrpcErrorCodeSystem:
				if subEc, err := strconv.ParseInt(re.Message[1:5], 0, 32); err == nil {
					//TODO return JsonRPC Error
					switch subEc {
					case ExpiredTransactionError:
						return ErrSendFailByExpired
					case FutureTransactionError:
						return ErrSendFailByFuture
					case TransactionPoolOverflowError:
						return ErrSendFailByOverflow
					}
				}
			case JsonrpcErrorCodePending, JsonrpcErrorCodeExecuting:
				return ErrGetResultFailByPending
			}
		case *common.HttpError:
			fmt.Printf("*common.HttpError:%+v", re)
			return ErrConnectFail
		case *url.Error:
			if common.IsConnectRefusedError(re.Err) {
				//fmt.Printf("*url.Error:%+v", re)
				return ErrConnectFail
			}
		}
	}
	return err
}

func mapErrorWithTransactionResult(txr *TransactionResult, err error) error {
	err = mapError(err)
	if err == nil && txr != nil && txr.Status != ResultStatusSuccess {
		fc, _ := txr.Failure.CodeValue.Value()
		if fc < ResultStatusFailureCodeRevert || fc > ResultStatusFailureCodeEnd {
			err = fmt.Errorf("failure with code:%s, message:%s",
				txr.Failure.CodeValue, txr.Failure.MessageValue)
		} else {
			err = NewRevertError(int(fc - ResultStatusFailureCodeRevert))
		}
	}
	return err
}
