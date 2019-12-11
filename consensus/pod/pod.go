// Copyright (C) 2017-2019 go-nebulas authors
//
// This file is part of the go-nebulas library.
//
// the go-nebulas library is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// the go-nebulas library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with the go-nebulas library.  If not, see <http://www.gnu.org/licenses/>.
//

package pod

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nebulasio/go-nebulas/util"

	lru "github.com/hashicorp/golang-lru"
	"github.com/nebulasio/go-nebulas/core"
	"github.com/nebulasio/go-nebulas/core/state"
	"github.com/nebulasio/go-nebulas/crypto/keystore"
	"github.com/nebulasio/go-nebulas/net"
	"github.com/nebulasio/go-nebulas/rpc"
	rpcpb "github.com/nebulasio/go-nebulas/rpc/pb"
	"github.com/nebulasio/go-nebulas/util/byteutils"
	"github.com/nebulasio/go-nebulas/util/logging"
	"github.com/sirupsen/logrus"
)

// PoD implementation of Proof-of-Devotion consensus
type PoD struct {
	quitCh chan bool

	chain *core.BlockChain
	ns    net.Service
	am    core.AccountManager

	dynasty *Dynasty

	coinbase               *core.Address
	miner                  *core.Address
	enableRemoteSignServer bool
	remoteSignServer       string

	messageCh chan net.Message

	slot       *lru.Cache
	reversible *lru.Cache

	enable     bool
	pending    bool
	launchBeat bool
}

// NewPoD create PoD.
func NewPoD() *PoD {
	pod := &PoD{
		quitCh:     make(chan bool, 5),
		enable:     false,
		pending:    true,
		launchBeat: false,
		messageCh:  make(chan net.Message, 128),
	}
	return pod
}

// Setup a pod consensus handler
func (pod *PoD) Setup(neblet core.Neblet) error {
	pod.chain = neblet.BlockChain()
	pod.ns = neblet.NetService()
	pod.am = neblet.AccountManager()

	dynasty, err := NewDynasty(neblet)
	if err != nil {
		return err
	}
	pod.dynasty = dynasty

	chainConfig := neblet.Config().Chain
	if chainConfig.StartMine {
		coinbase, err := core.AddressParse(chainConfig.Coinbase)
		if err != nil {
			logging.CLog().WithFields(logrus.Fields{
				"address": chainConfig.Coinbase,
				"err":     err,
			}).Error("Failed to parse coinbase address.")
			return err
		}
		miner, err := core.AddressParse(chainConfig.Miner)
		if err != nil {
			logging.CLog().WithFields(logrus.Fields{
				"address": chainConfig.Miner,
				"err":     err,
			}).Error("Failed to parse miner address.")
			return err
		}
		pod.coinbase = coinbase
		pod.miner = miner
		pod.enableRemoteSignServer = chainConfig.EnableRemoteSignServer
		pod.remoteSignServer = chainConfig.RemoteSignServer
	}

	slot, err := lru.New(128)
	if err != nil {
		return err
	}
	pod.slot = slot

	reversible, err := lru.New(128)
	if err != nil {
		return err
	}
	pod.reversible = reversible
	return nil
}

// Start start pod service.
func (pod *PoD) Start() {
	logging.CLog().Info("Starting pod Mining...")

	pod.ns.Register(net.NewSubscriber(pod, pod.messageCh, true, MessageTypeWitness, net.MessageWeightZero))
	go pod.blockLoop()
}

// Stop stop pod service.
func (pod *PoD) Stop() {
	logging.CLog().Info("Stopping pod Mining...")
	pod.ns.Deregister(net.NewSubscriber(pod, pod.messageCh, true, MessageTypeWitness, net.MessageWeightZero))
	pod.DisableMining()

	pod.quitCh <- true
}

// EnableMining start the consensus
func (pod *PoD) EnableMining(passphrase string) error {
	if err := pod.unlock(passphrase); err != nil {
		return err
	}
	pod.enable = true
	logging.CLog().Info("Enabled pod Mining...")
	return nil
}

// DisableMining stop the consensus
func (pod *PoD) DisableMining() error {
	if err := pod.am.Lock(pod.miner); err != nil {
		return err
	}
	pod.enable = false
	logging.CLog().Info("Disable pod Mining...")
	return nil
}

// Enable returns is mining
func (pod *PoD) Enable() bool {
	return pod.enable
}

func less(a *core.Block, b *core.Block) bool {
	if a.Height() != b.Height() {
		return a.Height() < b.Height()
	}
	return byteutils.Less(a.Hash(), b.Hash())
}

// ForkChoice select new tail
func (pod *PoD) ForkChoice() error {
	bc := pod.chain
	tailBlock := bc.TailBlock()
	detachedTailBlocks := bc.DetachedTailBlocks()

	// find the max depth.
	newTailBlock := tailBlock

	for _, v := range detachedTailBlocks {
		if less(newTailBlock, v) {
			newTailBlock = v
		}
	}

	if newTailBlock.Hash().Equals(tailBlock.Hash()) {
		logging.VLog().WithFields(logrus.Fields{
			"old tail": tailBlock,
			"new tail": newTailBlock,
		}).Debug("Current tail is best, no need to change.")
		return nil
	}

	err := bc.SetTailBlock(newTailBlock)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"new tail": newTailBlock,
			"old tail": tailBlock,
			"err":      err,
		}).Debug("Failed to set new tail block.")
		return err
	}

	logging.VLog().WithFields(logrus.Fields{
		"new tail": newTailBlock,
		"old tail": tailBlock,
	}).Info("change to new tail.")
	return nil
}

// UpdateLIB update the latest irrversible block
func (pod *PoD) UpdateLIB(rversibleBlocks []byteutils.Hash) {

	if pod.enable && core.NodeUpdateAtHeight(pod.chain.TailBlock().Height()) {
		found, _ := pod.dynasty.isProposer(pod.chain.TailBlock().Timestamp(), pod.miner.Bytes())
		if found {
			if err := pod.broadcastWitness(rversibleBlocks); err != nil {
				logging.VLog().WithFields(logrus.Fields{
					"err": err,
				}).Error("Failed to broadcast witness.")
			}
		}
	}

	lib := pod.chain.LIB()
	tail := pod.chain.TailBlock()
	cur := tail
	miners := make(map[string]bool)
	dynasty := int64(-1)
	for !cur.Hash().Equals(lib.Hash()) {
		curDynasty := cur.Timestamp() * SecondInMs / DynastyIntervalInMs
		if curDynasty != dynasty {
			miners = make(map[string]bool)
			dynasty = curDynasty
		}
		// fast prune
		if int(cur.Height())-int(lib.Height()) < ConsensusSize-len(miners) {
			return
		}
		miners[byteutils.Hex(cur.ConsensusRoot().Proposer)] = true
		if len(miners) >= ConsensusSize {
			pod.setLib(cur, len(miners))
			return
		}

		tmp := cur
		cur = pod.chain.GetBlock(cur.ParentHash())
		if cur == nil || core.CheckGenesisBlock(cur) {
			logging.VLog().WithFields(logrus.Fields{
				"tail": tail,
				"cur":  tmp,
			}).Debug("Failed to find latest irreversible block.")
			return
		}
	}

	logging.VLog().WithFields(logrus.Fields{
		"cur":              cur,
		"lib":              lib,
		"tail":             tail,
		"err":              "supported miners is not enough",
		"miners.limit":     ConsensusSize,
		"miners.supported": len(miners),
	}).Debug("Failed to update latest irreversible block.")
}

func (pod *PoD) setLib(block *core.Block, confirmed int) {
	if err := pod.chain.StoreLIBHashToStorage(block); err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"tail": pod.chain.TailBlock(),
			"lib":  block,
		}).Debug("Failed to store latest irreversible block.")
		return
	}
	logging.CLog().WithFields(logrus.Fields{
		"lib.new":          block,
		"lib.old":          pod.chain.LIB(),
		"tail":             pod.chain.TailBlock(),
		"miners.limit":     ConsensusSize,
		"miners.supported": confirmed,
	}).Info("Succeed to update latest irreversible block.")
	pod.chain.SetLIB(block)

	pod.reversible.Remove(block.Hash().Hex())

	e := &state.Event{
		Topic: core.TopicLibBlock,
		Data:  pod.chain.LIB().String(),
	}
	pod.chain.EventEmitter().Trigger(e)
}

// Pending return if consensus can do mining now
func (pod *PoD) Pending() bool {
	return pod.pending
}

// SuspendMining pend pod mining
func (pod *PoD) SuspendMining() {
	logging.CLog().Info("Suspended pod Mining.")
	pod.pending = true
}

// ResumeMining continue pod mining
func (pod *PoD) ResumeMining() {
	logging.CLog().Info("Resumed pod Mining.")
	pod.pending = false
}

func verifyBlockSign(miner *core.Address, block *core.Block) error {
	signer, err := core.RecoverSignerFromSignature(block.Alg(), block.Hash(), block.Signature())
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"signer": signer,
			"err":    err,
			"block":  block,
		}).Debug("Failed to recover block's miner.")
		return err
	}
	if !miner.Equals(signer) {
		logging.VLog().WithFields(logrus.Fields{
			"signer": signer,
			"miner":  miner,
			"block":  block,
		}).Debug("Failed to verify block's sign.")
		return ErrInvalidBlockProposer
	}
	return nil
}

// CheckDoubleMint if double mint exists
func (pod *PoD) CheckDoubleMint(block *core.Block) bool {
	if preBlock, exist := pod.slot.Get(block.Timestamp()); exist {
		if preBlock.(*core.Block).Hash().Equals(block.Hash()) == false {

			pod.reportEvil(preBlock.(*core.Block), block)

			logging.VLog().WithFields(logrus.Fields{
				"curBlock": block,
				"preBlock": preBlock.(*core.Block),
			}).Warn("Found someone minted multiple blocks at same time.")
			return true
		}
	}
	return false
}

func (pod *PoD) reportEvil(preBlock, block *core.Block) {
	if !pod.enable || !core.NodeUpdateAtHeight(pod.chain.TailBlock().Height()) {
		return
	}

	found, _ := pod.dynasty.isProposer(block.Timestamp(), pod.miner.Bytes())
	if found {
		evil := core.AttackNotMiner
		if preBlock.Miner().Equals(block.Miner()) {
			evil = core.AttackDoubleSpend
		}
		// submit double mint attack
		report := &core.Report{
			Timestamp: block.Timestamp(),
			Miner:     block.Miner().String(),
			Evil:      evil,
		}
		bytes, _ := report.ToBytes()
		err := pod.sendTransaction(block.Timestamp(), core.PoDReport, bytes)
		logging.VLog().WithFields(logrus.Fields{
			"curBlock": block,
			"preBlock": preBlock,
			"error":    err,
		}).Info("Found someone minted multiple blocks at same time.")
	} else {
		dynasty, _ := block.Dynasty()
		logging.VLog().WithFields(logrus.Fields{
			"timestamp": block.Timestamp(),
			"serial":    pod.dynasty.serial(block.Timestamp()),
			"miner":     pod.miner,
			"dynasty":   dynasty,
			"curBlock":  block,
			"preBlock":  preBlock,
		}).Info("Not the dynasty proposer.")
	}
}

// Serial return dynasty serial number
func (pod *PoD) Serial(timestamp int64) int64 {
	return pod.dynasty.serial(timestamp)
}

// VerifyBlock verify the block
func (pod *PoD) VerifyBlock(block *core.Block) error {
	tail := pod.chain.TailBlock()
	// check timestamp
	if block.Timestamp() != block.ConsensusRoot().Timestamp {
		return ErrInvalidBlockTimestamp
	}
	elapsedSecondInMs := block.Timestamp() * SecondInMs
	if elapsedSecondInMs <= 0 || (elapsedSecondInMs%BlockIntervalInMs) != 0 {
		return ErrInvalidBlockInterval
	}

	var (
		miners []byteutils.Hash
		err    error
	)

	cs, err := pod.dynasty.getDynasty(block.Timestamp())
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"err":   err,
			"tail":  tail,
			"block": block,
		}).Error("Failed to retrieve dynasty trie.")
		return err
	}
	miners, err = TraverseDynasty(cs)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"err":   err,
			"block": block,
		}).Debug("Failed to get miners from dynasty.")
		return err
	}
	proposer, err := FindProposer(block.Timestamp(), miners)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"proposer": proposer,
			"err":      err,
			"block":    block,
		}).Debug("Failed to find proposer.")
		return err
	}
	miner, err := core.AddressParseFromBytes(proposer)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"proposer": proposer,
			"err":      err,
			"block":    block,
		}).Debug("Failed to parse proposer.")
		return err
	}
	// check signature
	if err := verifyBlockSign(miner, block); err != nil {
		return err
	}

	// check block random
	if core.RandomAvailableAtHeight(block.Height()) && !block.HasRandomSeed() {
		logging.VLog().WithFields(logrus.Fields{
			"blockHeight":      block.Height(),
			"compatibleHeight": core.NebCompatibility.RandomAvailableHeight(),
		}).Debug("No random found in block header.")
		return core.ErrInvalidBlockRandom
	}

	pod.slot.Add(block.Timestamp(), block)
	return nil
}

func (pod *PoD) generateRandomSeed(block *core.Block) error {

	ancestorHash, parentSeed, err := pod.chain.GetInputForVRFSigner(block.ParentHash(), block.Height())
	if err != nil {
		return err
	}

	if pod.enableRemoteSignServer == true {
		conn, err := rpc.Dial(pod.remoteSignServer)
		if err != nil {
			return err
		}
		defer conn.Close()
		client := rpcpb.NewAdminServiceClient(conn)

		// generate VRF hash,proof
		random, err := client.GenerateRandomSeed(
			context.Background(),
			&rpcpb.GenerateRandomSeedRequest{
				Address:      pod.miner.String(),
				ParentSeed:   parentSeed,
				AncestorHash: ancestorHash,
			})
		if err != nil {
			return err
		}

		block.SetRandomSeed(random.VrfSeed, random.VrfProof)
	} else {
		// generate VRF hash,proof
		vrfSeed, vrfProof, err := pod.am.GenerateRandomSeed(pod.miner, ancestorHash, parentSeed)
		if err != nil {
			return err
		}
		block.SetRandomSeed(vrfSeed, vrfProof)
	}
	return nil
}

func (pod *PoD) signBlock(block *core.Block) error {
	if pod.enableRemoteSignServer {
		alg := keystore.SECP256K1
		sign, err := pod.remoteSign(alg, block.Hash())
		if err != nil {
			return err
		}
		block.SetSignature(alg, sign)
		return nil
	} else {
		return pod.am.SignBlock(pod.miner, block)
	}
}

func (pod *PoD) remoteSign(alg keystore.Algorithm, hash byteutils.Hash) (byteutils.Hash, error) {
	conn, err := rpc.Dial(pod.remoteSignServer)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	remoteSignClient := rpcpb.NewAdminServiceClient(conn)

	result, err := remoteSignClient.SignHash(context.Background(),
		&rpcpb.SignHashRequest{
			Address: pod.miner.String(),
			Hash:    hash,
			Alg:     uint32(alg),
		})
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (pod *PoD) unlock(passphrase string) error {
	if pod.enableRemoteSignServer == false {
		return pod.am.Unlock(pod.miner, []byte(passphrase), DefaultMaxUnlockDuration)
	}
	return nil

}

func (pod *PoD) newBlock(tail *core.Block, consensusState state.ConsensusState, deadlineInMs int64) (*core.Block, error) {
	startAt := time.Now().Unix()
	block, err := core.NewBlock(pod.chain.ChainID(), pod.coinbase, tail)
	if err != nil {
		logging.CLog().WithFields(logrus.Fields{
			"tail":     tail,
			"coinbase": pod.coinbase,
			"chainid":  pod.chain.ChainID(),
			"err":      err,
		}).Error("Failed to create new block")
		return nil, err
	}

	if core.RandomAvailableAtHeight(block.Height()) {
		if err := pod.generateRandomSeed(block); err != nil {
			logging.VLog().WithFields(logrus.Fields{
				"block": block,
				"err":   err,
			}).Error("Failed to generate random seed from remote.")
			return nil, err
		}
	}

	block.WorldState().SetConsensusState(consensusState)
	block.SetTimestamp(consensusState.TimeStamp())
	block.CollectTransactions(deadlineInMs)
	if err = block.Seal(); err != nil {
		logging.CLog().WithFields(logrus.Fields{
			"block": block,
			"err":   err,
		}).Error("Failed to seal new block")
		go block.ReturnTransactions()
		return nil, err
	}

	if err := pod.signBlock(block); err != nil {
		logging.CLog().WithFields(logrus.Fields{
			"miner": pod.miner,
			"block": block,
			"err":   err,
		}).Error("Failed to sign new block")
		go block.ReturnTransactions()
		return nil, err
	}
	endAt := time.Now().Unix()

	logging.VLog().WithFields(logrus.Fields{
		"start": startAt,
		"end":   endAt,
		"diff":  endAt - startAt,
		"block": block,
		"txs":   len(block.Transactions()),
	}).Debug("Packed txs.")

	return block, nil
}

func lastSlot(nowInMs int64) int64 {
	return int64((nowInMs-SecondInMs)/BlockIntervalInMs) * BlockIntervalInMs
}

func nextSlot(nowInMs int64) int64 {
	return int64((nowInMs+BlockIntervalInMs-SecondInMs)/BlockIntervalInMs) * BlockIntervalInMs
}

func deadline(nowInMs int64) int64 {
	nextSlotInMs := nextSlot(nowInMs)
	remainInMs := nextSlotInMs - nowInMs
	if MaxMintDurationInMs > remainInMs {
		return nextSlotInMs
	}
	return nowInMs + MaxMintDurationInMs
}

func (pod *PoD) checkDeadline(tail *core.Block, nowInMs int64) (int64, error) {
	lastSlotInMs := lastSlot(nowInMs)
	nextSlotInMs := nextSlot(nowInMs)

	if tail.Timestamp()*SecondInMs >= nextSlotInMs {
		return 0, ErrBlockMintedInNextSlot
	}
	if tail.Timestamp()*SecondInMs == lastSlotInMs {
		return deadline(nowInMs), nil
	}
	if nextSlotInMs-nowInMs <= MinMintDurationInMs {
		return deadline(nowInMs), nil
	}
	return 0, ErrWaitingBlockInLastSlot
}

func (pod *PoD) checkProposer(tail *core.Block, nowInMs int64) (state.ConsensusState, error) {
	slotInMs := nextSlot(nowInMs)
	elapsedInMs := slotInMs - tail.Timestamp()*SecondInMs
	consensusState, err := tail.WorldState().NextConsensusState(elapsedInMs / SecondInMs)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"tail":    tail,
			"elapsed": elapsedInMs,
			"err":     err,
		}).Debug("Failed to generate next dynasty context.")
		return nil, ErrGenerateNextConsensusState
	}
	if consensusState.Proposer() == nil || !consensusState.Proposer().Equals(pod.miner.Bytes()) {
		proposer := "nil"
		if consensusState.Proposer() != nil {
			proposer = consensusState.Proposer().Base58()
		}
		logging.VLog().WithFields(logrus.Fields{
			"tail":     tail,
			"now":      nowInMs,
			"slot":     slotInMs,
			"expected": proposer,
			"actual":   pod.miner,
		}).Debug("Not my turn, waiting...")
		return nil, ErrInvalidBlockProposer
	}
	return consensusState, nil
}

func (pod *PoD) pushAndBroadcast(tail *core.Block, block *core.Block) error {
	if err := pod.chain.BlockPool().PushAndBroadcast(block); err != nil {
		logging.CLog().WithFields(logrus.Fields{
			"tail":  tail,
			"block": block,
			"err":   err,
		}).Error("Failed to push new minted block into block pool")
		return err
	}

	if !pod.chain.TailBlock().Hash().Equals(block.Hash()) {
		return ErrAppendNewBlockFailed
	}

	logging.CLog().WithFields(logrus.Fields{
		"tail":  tail,
		"block": block,
	}).Info("Broadcasted new block")
	return nil
}

func (pod *PoD) mintBlock(now int64) error {
	metricsBlockPackingTime.Update(0)
	metricsBlockWaitingTime.Update(0)

	nowInMs := now * SecondInMs
	// check mining enable
	if !pod.enable {
		return ErrCannotMintWhenDisable
	}

	// check mining pending
	if pod.pending {
		return ErrCannotMintWhenPending
	}

	tail := pod.chain.TailBlock()

	deadlineInMs, err := pod.checkDeadline(tail, nowInMs)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"tail": tail,
			"now":  nowInMs,
			"err":  err,
		}).Debug("checkDeadline")
		return err
	}

	consensusState, err := pod.checkProposer(tail, nowInMs)
	if err != nil {
		return err
	}

	miner := "nil"
	if pod.miner != nil {
		miner = pod.miner.String()
	}
	logging.CLog().WithFields(logrus.Fields{
		"tail":     tail,
		"start":    nowInMs,
		"deadline": deadlineInMs,
		"expected": consensusState.Proposer().Hex(),
		"actual":   miner,
	}).Info("My turn to mint block")
	metricsBlockPackingTime.Update(deadlineInMs - nowInMs)

	err = pod.triggerState(now)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"timestamp": now,
			"serial":    pod.dynasty.serial(now),
			"err":       err,
		}).Error("Failed to trigger state.")
	}

	block, err := pod.newBlock(tail, consensusState, deadlineInMs)
	if err != nil {
		return err
	}

	slotInMs := nextSlot(nowInMs)
	currentInMs := time.Now().Unix() * SecondInMs
	if slotInMs > currentInMs {
		timer := time.NewTimer(time.Duration(slotInMs-currentInMs) * time.Millisecond).C
		<-timer
		metricsBlockWaitingTime.Update(slotInMs - currentInMs)
	}

	logging.CLog().WithFields(logrus.Fields{
		"tail":     tail,
		"block":    block,
		"start":    nowInMs,
		"packed":   currentInMs,
		"deadline": deadlineInMs,
		"slot":     slotInMs,
		"end":      time.Now().Unix(),
	}).Info("Minted new block")

	metricsMintBlock.Inc(1)
	// try to push the new block on chain
	// if failed, return all txs back

	if err := pod.pushAndBroadcast(tail, block); err != nil {
		go block.ReturnTransactions()
		return err
	}

	return nil
}

func (pod *PoD) heartbeat(now int64) error {
	// check mining enable
	if !pod.enable {
		return ErrNoHeartbeatWhenDisable
	}

	if !core.NodeUpdateAtHeight(pod.chain.TailBlock().Height()) {
		return nil
	}

	if pod.launchBeat {
		nowInMs := now * SecondInMs
		// only heartbeat once in a interval
		if (nowInMs+DynastyIntervalInMs/2)%DynastyIntervalInMs != 0 {
			return nil
		}
	}
	pod.launchBeat = true

	participants, err := pod.dynasty.getParticipants()
	if err != nil {
		return err
	}

	minerSignUp := false
	miner := pod.miner.String()
	for _, v := range participants {
		if miner == v {
			minerSignUp = true
			break
		}
	}

	if minerSignUp {
		err = pod.sendTransaction(now, core.PoDHeartbeat, nil)
	} else {
		err = ErrMinerNotSignUp
	}

	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"miner":     pod.miner.String(),
			"timestamp": now,
			"err":       err,
		}).Error("Failed to send heartbeat")
	} else {
		logging.VLog().WithFields(logrus.Fields{
			"miner":     pod.miner.String(),
			"timestamp": now,
		}).Info("send miner heartbeat")
	}

	return err
}

// triggerState trigger the pod contract state machine
// if next serial dynasty not found, we need generate it
// and submit last serial block mint statics
func (pod *PoD) triggerState(now int64) error {
	if !pod.enable || !core.NodeUpdateAtHeight(pod.chain.TailBlock().Height()) {
		return nil
	}

	logging.VLog().WithFields(logrus.Fields{
		"miner":     pod.miner.String(),
		"timestamp": now,
	}).Debug("trigger state")

	serial := pod.dynasty.serial(now)
	if pod.dynasty.tries[serial+1] == nil {
		if err := pod.dynasty.loadFromContract(serial); err != nil {
			return err
		}
	}
	if pod.dynasty.tries[serial+1] == nil {
		states, err := pod.chain.StatisticalLastBlocks(serial)
		if err != nil {
			return err
		}
		bytes, err := json.Marshal(states)
		if err != nil {
			return err
		}
		err = pod.sendTransaction(now, core.PoDState, bytes)
		if err != nil {
			return err
		}
	}
	return nil
}

func (pod *PoD) blockLoop() {
	logging.CLog().Info("Started pod Mining.")
	timeChan := time.NewTicker(time.Second).C
	for { // ToRefine: change loop logic, try more times second
		select {
		case now := <-timeChan:
			metricsLruPoolSlotBlock.Update(int64(pod.slot.Len()))
			timestamp := now.Unix()
			pod.heartbeat(timestamp)
			pod.mintBlock(timestamp)
		case <-pod.quitCh:
			logging.CLog().Info("Stopped pod Mining.")
			return
		case message := <-pod.messageCh:
			switch message.MessageType() {
			case MessageTypeWitness:
				pod.onWitnessReceived(message)
			default:
				logging.VLog().WithFields(logrus.Fields{
					"messageName": message.MessageType(),
				}).Warn("Received unknown message.")
			}
		}
	}
}

func (pod *PoD) findProposer(now int64) (proposer byteutils.Hash, err error) {
	miners, err := pod.chain.TailBlock().WorldState().Dynasty()
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"err": err,
		}).Debug("Failed to get miners from dynasty.")
		return nil, err
	}
	proposer, err = FindProposer(now, miners)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"proposer": proposer,
			"err":      err,
		}).Debug("Failed to find proposer.")
		return nil, err
	}
	return proposer, nil
}

// NumberOfBlocksInDynasty number of blocks in one dynasty
func (pod *PoD) NumberOfBlocksInDynasty() uint64 {
	return uint64(DynastyIntervalInMs) / uint64(BlockIntervalInMs)
}

// sendTransaction send pod consensus transaction
func (pod *PoD) sendTransaction(timestamp int64, action string, data []byte) error {
	payload, err := core.NewPodPayload(pod.dynasty.serial(timestamp), action, data)
	if err != nil {
		return err
	}
	bytes, err := payload.ToBytes()
	if err != nil {
		return err
	}
	acc, err := pod.chain.TailBlock().GetAccount(pod.miner.Bytes())
	if err != nil {
		return err
	}
	nonce := acc.Nonce() + 1
	tx, err := core.NewTransaction(pod.chain.ChainID(), pod.miner, core.PoDContract, util.NewUint128(), nonce, core.TxPayloadPodType, bytes, core.TransactionMaxGasPrice, core.TransactionMaxGas)
	if err != nil {
		return err
	}
	tx.SetTimestamp(timestamp)
	hash, err := tx.HashTransaction()
	if err != nil {
		return err
	}
	tx.SetHash(hash)

	if err := pod.signTransaction(tx); err != nil {
		return err
	}

	return pod.chain.TransactionPool().PushAndBroadcast(tx)
}

func (pod *PoD) signTransaction(tx *core.Transaction) error {
	if pod.enableRemoteSignServer {
		alg := keystore.SECP256K1
		sign, err := pod.remoteSign(alg, tx.Hash())
		if err != nil {
			return err
		}
		tx.SetSignature(alg, sign)
		return nil
	} else {
		return pod.am.SignTransaction(pod.miner, tx)
	}
}
