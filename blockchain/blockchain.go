// Copyright (C) 2017, Zipper Team.  All rights reserved.
//
// This file is part of zipper
//
// The zipper is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The zipper is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package blockchain

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/willf/bloom"
	"github.com/zipper-project/zipper/account"
	"github.com/zipper-project/zipper/blockchain/validator"
	"github.com/zipper-project/zipper/common/crypto"
	"github.com/zipper-project/zipper/common/db"
	"github.com/zipper-project/zipper/common/log"
	"github.com/zipper-project/zipper/config"
	"github.com/zipper-project/zipper/consensus"
	"github.com/zipper-project/zipper/consensus/consenter"
	"github.com/zipper-project/zipper/ledger"
	"github.com/zipper-project/zipper/ledger/balance"
	"github.com/zipper-project/zipper/ledger/state"
	"github.com/zipper-project/zipper/peer"
	p2p "github.com/zipper-project/zipper/peer/proto"
	"github.com/zipper-project/zipper/proto"
)

// Blockchain is blockchain instance
type Blockchain struct {
	mu sync.Mutex
	wg sync.WaitGroup

	currentBlockHeader *proto.BlockHeader

	filter *bloom.BloomFilter

	//ledger
	ledger *ledger.Ledger

	// validator
	validator validator.Validator

	// consensus
	consenter consensus.Consenter

	// server
	server *peer.Server

	quitCh chan bool

	orphans *list.List

	// 0 respresents sync block, 1 respresents sync done
	synced  bool
	started bool
}

var (
	filterN       uint = 1000000
	falsePositive      = 0.000001
)

// load loads local blockchain data
func (bc *Blockchain) load() {
	t := time.Now()
	bc.ledger.VerifyChain()
	delay := time.Since(t)

	height, err := bc.ledger.Height()

	if err != nil {
		log.Error("GetBlockHeight error", err)
		return
	}
	bc.currentBlockHeader, err = bc.ledger.GetBlockByNumber(height)

	if bc.currentBlockHeader == nil || err != nil {
		log.Errorf("GetBlockByNumber error %v ", err)
		panic(err)
	}

	log.Debugf("Load blockchain data, bestblockhash: %s height: %d load delay : %v ", bc.currentBlockHeader.Hash(), height, delay)
}

// NewBlockchain returns a fully initialised blockchain service using input data
func NewBlockchain(pm peer.IProtocolManager) *Blockchain {
	bc := &Blockchain{
		mu:                 sync.Mutex{},
		wg:                 sync.WaitGroup{},
		currentBlockHeader: new(proto.BlockHeader),
		filter:             bloom.NewWithEstimates(filterN, falsePositive),
		orphans:            list.New(),
		quitCh:             make(chan bool),
	}

	log.Debugf("start: db.NewDB...")
	chainDb := db.NewDB(config.DBConfig())

	log.Debugf("start: ledger.NewLedger...")
	bc.ledger = ledger.NewLedger(chainDb)
	bc.load()

	log.Debugf("start: consenter.NewConsenter...")
	bc.consenter = consenter.NewConsenter(config.ConsenterOptions(), bc)

	bc.validator = validator.NewVerification(config.ValidatorConfig(), bc.ledger, bc.consenter)

	log.Debugf("start: peer.NewServer...")
	bc.server = peer.NewServer(config.ServerOption(), pm)
	return bc
}

func (bc *Blockchain) Start() {
	if bc.consenter.Name() == "noops" {
		bc.StartServices()
	}
	go bc.server.Start()
	go func() {
		for {
			select {
			case <-bc.quitCh:
				return
			case broadcastConsensusData := <-bc.consenter.BroadcastConsensusChannel():
				//TODO
				header := &p2p.Header{}
				header.ProtoID = 1
				header.MsgID = 1
				msg := p2p.NewMessage(header, broadcastConsensusData.Payload)
				bc.server.Broadcast(msg, peer.VP)
			case commitedTxs := <-bc.consenter.OutputTxsChannel():
				//add lo
				log.Infof("Outputs StartConsensusService len=%d", len(commitedTxs.Txs))

				height, _ := bc.ledger.Height()
				height++
				if commitedTxs.Height == height {
					if !bc.synced {
						bc.synced = true
					}
					bc.processConsensusOutput(commitedTxs)
				} else if commitedTxs.Height > height {
					//orphan
					bc.orphans.PushBack(commitedTxs)
					for elem := bc.orphans.Front(); elem != nil; elem = elem.Next() {
						ocommitedTxs := elem.Value.(*consensus.OutputTxs)
						if ocommitedTxs.Height < height {
							bc.orphans.Remove(elem)
						} else if ocommitedTxs.Height == height {
							bc.orphans.Remove(elem)
							bc.processConsensusOutput(ocommitedTxs)
							height++
						} else {
							break
						}
					}
					if bc.orphans.Len() > 100 {
						bc.orphans.Remove(bc.orphans.Front())
					}
				} /*else if bc.synced {
					log.Panicf("Height %d already exist in ledger", commitedTxs.Height)
				}*/
			}
		}
	}()
}

func (bc *Blockchain) Stop() {
	//TODO
	close(bc.quitCh)
	bc.server.Stop()
}

// CurrentHeight returns current heigt of the current block
func (bc *Blockchain) CurrentHeight() uint32 {
	return bc.currentBlockHeader.Height
}

// CurrentBlockHash returns current block hash of the current block
func (bc *Blockchain) CurrentBlockHash() crypto.Hash {
	return bc.currentBlockHeader.Hash()
}

// GetNextBlockHash returns the next block hash
func (bc *Blockchain) GetNextBlockHash(h crypto.Hash) (crypto.Hash, error) {
	blockHeader, err := bc.ledger.GetBlockByHash(h.Bytes())
	if blockHeader == nil || err != nil {
		return h, err
	}
	nextBlockHeader, err := bc.ledger.GetBlockByNumber(blockHeader.Height + 1)
	if nextBlockHeader == nil || err != nil {
		return h, err
	}
	hash := nextBlockHeader.Hash()
	return hash, nil
}

// GetAsset returns asset
func (bc *Blockchain) GetAsset(id uint32) *state.Asset {
	if bc.validator == nil {
		b, _ := bc.ledger.GetAsset(id)
		return b
	}
	return bc.validator.GetAsset(id)
}

// GetBalance returns balance
func (bc *Blockchain) GetBalance(addr account.Address) *balance.Balance {
	if bc.validator == nil {
		b, _ := bc.ledger.GetBalance(addr)
		return b
	}
	return bc.validator.GetBalance(addr)
}

// GetTransaction returns transaction in ledger first then txBool
func (bc *Blockchain) GetTransaction(txHash crypto.Hash) (*proto.Transaction, error) {
	tx, err := bc.ledger.GetTxByTxHash(txHash.Bytes())
	if bc.validator != nil && err != nil {
		var ok bool
		if tx, ok = bc.validator.GetTransactionByHash(txHash); ok {
			return tx, nil
		}
	}

	return tx, err
}

// Start starts blockchain services
func (bc *Blockchain) StartServices() {
	// start consesnus
	bc.StartConsensusService()
	// start txpool
	bc.StartTxPoolService()
	log.Debug("BlockChain Service start")
	bc.started = true
}

func (bc *Blockchain) Started() bool {
	return bc.started
}

func (bc *Blockchain) Synced() bool {
	return bc.synced
}

// StartConsensusService starts consensus service
func (bc *Blockchain) StartConsensusService() {
	go bc.consenter.Start()
}

func (bc *Blockchain) processConsensusOutput(output *consensus.OutputTxs) {
	blk := bc.GenerateBlock(output.Txs, output.Time)
	if blk.Height() == output.Height {
		bc.Relay(blk)
	}
}

// StartTxPool starts txpool service
func (bc *Blockchain) StartTxPoolService() {
	bc.validator.Start()
}

// ProcessTransaction processes new transaction from the network
func (bc *Blockchain) ProcessTransaction(tx *proto.Transaction, needNotify bool) bool {
	// step 1: validate and mark transaction
	// step 2: add transaction to txPool
	// if atomic.LoadUint32(&bc.synced) == 0 {
	log.Debugf("[Blockchain] new tx, tx_hash: %v, tx_sender: %v, tx_nonce: %v", tx.Hash().String(), tx.Sender().String(), tx.Nonce())
	if bc.validator == nil {
		return true
	}

	err := bc.validator.ProcessTransaction(tx)
	log.Debugf("[Blockchain] new tx, tx_hash: %v, tx_sender: %v, tx_nonce: %v, end", tx.Hash().String(), tx.Sender().String(), tx.Nonce())
	if err != nil {
		log.Errorf(fmt.Sprintf("process transaction %v failed, %v", tx.Hash(), err))
		return false
	}

	return true
}

// ProcessBlock processes new block from the network,flag = true pack up block ,flag = false sync block
func (bc *Blockchain) ProcessBlock(blk *proto.Block, flag bool) bool {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	log.Debugf("block previoushash %s, currentblockhash %s,len %d", blk.PreviousHash(), bc.CurrentBlockHash(), len(blk.Transactions))
	if blk.PreviousHash() == bc.CurrentBlockHash().String() {
		bc.ledger.AppendBlock(blk, flag)
		log.Infof("New Block  %s, height: %d Transaction Number: %d", blk.Hash(), blk.Height(), len(blk.Transactions))
		bc.currentBlockHeader = blk.Header
		return true
	}
	return false
}

func (bc *Blockchain) merkleRootHash(txs []*proto.Transaction) crypto.Hash {
	if len(txs) > 0 {
		hashs := make([]crypto.Hash, 0)
		for _, tx := range txs {
			hashs = append(hashs, tx.Hash())
		}
		return crypto.ComputeMerkleHash(hashs)[0]
	}
	return crypto.Hash{}
}

// GenerateBlock gets transactions from consensus service and generates a new block
func (bc *Blockchain) GenerateBlock(txs proto.Transactions, createTime uint32) *proto.Block {
	var (
		// default value is empty hash
		merkleRootHash crypto.Hash
		stateRootHash  crypto.Hash
	)

	blk := proto.NewBlock(bc.currentBlockHeader.Hash(), stateRootHash,
		createTime, bc.currentBlockHeader.Height+1,
		uint32(100),
		merkleRootHash,
		txs,
	)
	return blk
}

func (bc *Blockchain) Relay(inv proto.IInventory) {
	var (
		msg    *p2p.Message
		invMsg = &proto.GetInvMsg{}
	)
	switch inv.(type) {
	case *proto.Transaction:
		tx := inv.(*proto.Transaction)
		if bc.filter.TestAndAdd(tx.Serialize()) {
			log.Debugf("Bloom Test is true, txHash: %+v", tx.Hash())
			return
		}
		if bc.ProcessTransaction(tx, true) {
			log.Debugf("ProcessTransaction, tx_hash: %+v", tx.Hash())
			invMsg.Type = proto.InvType_transaction
			invMsg.Hashs = []string{inv.Hash().String()}

			header := &p2p.Header{}
			header.ProtoID = 1
			header.MsgID = 1
			imsgByte, _ := invMsg.MarshalMsg()
			msg = p2p.NewMessage(header, imsgByte)
		}
	case *proto.Block:
		block := inv.(*proto.Block)
		if bc.filter.TestAndAdd(block.Serialize()) {
			log.Debugf("Bloom Test is true, BlockHash: %+v", block.Hash())
			return
		}

		if bc.ProcessBlock(block, true) {
			log.Debugf("Relay inventory %v", inv)
			invMsg.Type = proto.InvType_block
			invMsg.Hashs = []string{inv.Hash().String()}

			header := &p2p.Header{}
			header.ProtoID = 1
			header.MsgID = 1
			imsgByte, _ := invMsg.MarshalMsg()
			msg = p2p.NewMessage(header, imsgByte)
		}
	}
	if msg != nil {
		bc.server.Broadcast(msg, peer.ALL)
	}
}
