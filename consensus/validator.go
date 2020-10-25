package consensus

import (
	"bytes"
	"encoding/hex"
	"time"

	"github.com/harmony-one/harmony/crypto/bls"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	msg_pb "github.com/harmony-one/harmony/api/proto/message"
	"github.com/harmony-one/harmony/consensus/signature"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/p2p"
)

func (consensus *Consensus) onAnnounce(msg *msg_pb.Message) {
	recvMsg, err := consensus.ParseFBFTMessage(msg)
	if err != nil {
		consensus.getLogger().Error().
			Err(err).
			Uint64("MsgBlockNum", recvMsg.BlockNum).
			Msg("[OnAnnounce] Unparseable leader message")
		return
	}

	// NOTE let it handle its own logs
	if !consensus.onAnnounceSanityChecks(recvMsg) {
		return
	}

	consensus.getLogger().Debug().
		Uint64("MsgViewID", recvMsg.ViewID).
		Uint64("MsgBlockNum", recvMsg.BlockNum).
		Msg("[OnAnnounce] Announce message Added")
	consensus.FBFTLog.AddMessage(recvMsg)
	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()
	consensus.blockHash = recvMsg.BlockHash
	// we have already added message and block, skip check viewID
	// and send prepare message if is in ViewChanging mode
	if consensus.IsViewChangingMode() {
		consensus.getLogger().Debug().
			Msg("[OnAnnounce] Still in ViewChanging Mode, Exiting !!")
		return
	}

	if consensus.checkViewID(recvMsg) != nil {
		if consensus.current.Mode() == Normal {
			consensus.getLogger().Debug().
				Uint64("MsgViewID", recvMsg.ViewID).
				Uint64("MsgBlockNum", recvMsg.BlockNum).
				Msg("[OnAnnounce] ViewID check failed")
		}
		return
	}
	consensus.StartFinalityCount()
	consensus.prepare()
}

func (consensus *Consensus) prepare() {
	priKeys := consensus.getPriKeysInCommittee()

	p2pMsgs := consensus.constructP2pMessages(msg_pb.MessageType_PREPARE, nil, priKeys)

	if err := consensus.broadcastConsensusP2pMessages(p2pMsgs); err != nil {
		consensus.getLogger().Warn().Err(err).Msg("[OnAnnounce] Cannot send prepare message")
	} else {
		consensus.getLogger().Info().
			Str("blockHash", hex.EncodeToString(consensus.blockHash[:])).
			Msg("[OnAnnounce] Sent Prepare Message!!")
	}

	consensus.switchPhase("Announce", FBFTPrepare)
}

// sendCommitMessages send out commit messages to leader
func (consensus *Consensus) sendCommitMessages(blockObj *types.Block) {
	priKeys := consensus.getPriKeysInCommittee()

	// Sign commit signature on the received block and construct the p2p messages
	commitPayload := signature.ConstructCommitPayload(consensus.Blockchain,
		blockObj.Epoch(), blockObj.Hash(), blockObj.NumberU64(), blockObj.Header().ViewID().Uint64())

	p2pMsgs := consensus.constructP2pMessages(msg_pb.MessageType_COMMIT, commitPayload, priKeys)

	if err := consensus.broadcastConsensusP2pMessages(p2pMsgs); err != nil {
		consensus.getLogger().Warn().Err(err).Msg("[sendCommitMessages] Cannot send commit message!!")
	} else {
		consensus.getLogger().Info().
			Uint64("blockNum", consensus.blockNum).
			Hex("blockHash", consensus.blockHash[:]).
			Msg("[sendCommitMessages] Sent Commit Message!!")
	}
}

// if onPrepared accepts the prepared message from the leader, then
// it will send a COMMIT message for the leader to receive on the network.
func (consensus *Consensus) onPrepared(msg *msg_pb.Message) {
	recvMsg, err := consensus.ParseFBFTMessage(msg)
	if err != nil {
		consensus.getLogger().Debug().Err(err).Msg("[OnPrepared] Unparseable validator message")
		return
	}
	consensus.getLogger().Info().
		Uint64("MsgBlockNum", recvMsg.BlockNum).
		Uint64("MsgViewID", recvMsg.ViewID).
		Msg("[OnPrepared] Received prepared message")

	if recvMsg.BlockNum < consensus.blockNum {
		consensus.getLogger().Debug().Uint64("MsgBlockNum", recvMsg.BlockNum).
			Msg("Wrong BlockNum Received, ignoring!")
		return
	}
	if recvMsg.BlockNum > consensus.blockNum {
		consensus.getLogger().Warn().Msgf("[OnPrepared] low consensus block number. Spin sync")
		consensus.spinUpStateSync()
	}

	// check validity of prepared signature
	blockHash := recvMsg.BlockHash
	aggSig, mask, err := consensus.ReadSignatureBitmapPayload(recvMsg.Payload, 0)
	if err != nil {
		consensus.getLogger().Error().Err(err).Msg("ReadSignatureBitmapPayload failed!")
		return
	}
	if !consensus.Decider.IsQuorumAchievedByMask(mask) {
		consensus.getLogger().Warn().Msgf("[OnPrepared] Quorum Not achieved.")
		return
	}
	if !aggSig.VerifyHash(mask.AggregatePublic, blockHash[:]) {
		myBlockHash := common.Hash{}
		myBlockHash.SetBytes(consensus.blockHash[:])
		consensus.getLogger().Warn().
			Uint64("MsgBlockNum", recvMsg.BlockNum).
			Uint64("MsgViewID", recvMsg.ViewID).
			Msg("[OnPrepared] failed to verify multi signature for prepare phase")
		return
	}

	// check validity of block
	var blockObj types.Block
	if err := rlp.DecodeBytes(recvMsg.Block, &blockObj); err != nil {
		consensus.getLogger().Warn().
			Err(err).
			Uint64("MsgBlockNum", recvMsg.BlockNum).
			Msg("[OnPrepared] Unparseable block header data")
		return
	}
	// let this handle it own logs
	if !consensus.onPreparedSanityChecks(&blockObj, recvMsg) {
		return
	}
	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()

	// tryCatchup is also run in onCommitted(), so need to lock with commitMutex.
	if consensus.current.Mode() != Normal {
		// don't sign the block that is not verified
		consensus.getLogger().Info().Msg("[OnPrepared] Not in normal mode, Exiting!!")
		return
	}
	if consensus.BlockVerifier == nil {
		consensus.getLogger().Debug().Msg("[onPrepared] consensus received message before init. Ignoring")
		return
	}
	if err := consensus.BlockVerifier(&blockObj); err != nil {
		consensus.getLogger().Error().Err(err).Msg("[OnPrepared] Block verification failed")
		return
	}
	consensus.FBFTLog.MarkBlockVerified(&blockObj)

	consensus.FBFTLog.AddBlock(&blockObj)
	// add block field
	blockPayload := make([]byte, len(recvMsg.Block))
	copy(blockPayload[:], recvMsg.Block[:])
	consensus.block = blockPayload
	recvMsg.Block = []byte{} // save memory space
	consensus.FBFTLog.AddMessage(recvMsg)
	consensus.getLogger().Debug().
		Uint64("MsgViewID", recvMsg.ViewID).
		Uint64("MsgBlockNum", recvMsg.BlockNum).
		Hex("blockHash", recvMsg.BlockHash[:]).
		Msg("[OnPrepared] Prepared message and block added")

	if consensus.checkViewID(recvMsg) != nil {
		if consensus.current.Mode() == Normal {
			consensus.getLogger().Debug().
				Uint64("MsgViewID", recvMsg.ViewID).
				Uint64("MsgBlockNum", recvMsg.BlockNum).
				Msg("[OnPrepared] ViewID check failed")
		}
		return
	}
	if recvMsg.BlockNum > consensus.blockNum {
		consensus.getLogger().Debug().
			Uint64("MsgBlockNum", recvMsg.BlockNum).
			Uint64("blockNum", consensus.blockNum).
			Msg("[OnPrepared] Future Block Received, ignoring!!")
		return
	}

	// this is a temp fix for allows FN nodes to earning reward
	if consensus.delayCommit > 0 {
		time.Sleep(consensus.delayCommit)
	}

	// add preparedSig field
	consensus.aggregatedPrepareSig = aggSig
	consensus.prepareBitmap = mask

	// Optimistically add blockhash field of prepare message
	emptyHash := [32]byte{}
	if bytes.Equal(consensus.blockHash[:], emptyHash[:]) {
		copy(consensus.blockHash[:], blockHash[:])
	}

	consensus.sendCommitMessages(&blockObj)
	consensus.switchPhase("onPrepared", FBFTCommit)
}

func (consensus *Consensus) onCommitted(msg *msg_pb.Message) {
	recvMsg, err := consensus.ParseFBFTMessage(msg)
	if err != nil {
		consensus.getLogger().Warn().Msg("[OnCommitted] unable to parse msg")
		return
	}
	// It's ok to receive committed message for last block due to pipelining.
	// The committed message for last block could include more signatures now.
	if recvMsg.BlockNum < consensus.blockNum-1 {
		consensus.getLogger().Debug().
			Uint64("MsgBlockNum", recvMsg.BlockNum).
			Msg("Wrong BlockNum Received, ignoring!")
		return
	}
	if recvMsg.BlockNum > consensus.blockNum {
		consensus.getLogger().Info().Msg("[OnCommitted] low consensus block number. Spin up state sync")
		consensus.spinUpStateSync()
	}

	aggSig, mask, err := consensus.ReadSignatureBitmapPayload(recvMsg.Payload, 0)
	if err != nil {
		consensus.getLogger().Error().Err(err).Msg("[OnCommitted] readSignatureBitmapPayload failed")
		return
	}
	if !consensus.Decider.IsQuorumAchievedByMask(mask) {
		consensus.getLogger().Warn().Msgf("[OnCommitted] Quorum Not achieved.")
		return
	}

	// Must have the corresponding block to verify committed message.
	blockObj := consensus.FBFTLog.GetBlockByHash(recvMsg.BlockHash)
	if blockObj == nil {
		consensus.getLogger().Debug().
			Uint64("blockNum", recvMsg.BlockNum).
			Uint64("viewID", recvMsg.ViewID).
			Str("blockHash", recvMsg.BlockHash.Hex()).
			Msg("[OnCommitted] Failed finding a matching block for committed message")
		return
	}
	commitPayload := signature.ConstructCommitPayload(consensus.Blockchain,
		blockObj.Epoch(), blockObj.Hash(), blockObj.NumberU64(), blockObj.Header().ViewID().Uint64())
	if !aggSig.VerifyHash(mask.AggregatePublic, commitPayload) {
		consensus.getLogger().Error().
			Uint64("MsgBlockNum", recvMsg.BlockNum).
			Msg("[OnCommitted] Failed to verify the multi signature for commit phase")
		return
	}

	consensus.FBFTLog.AddMessage(recvMsg)

	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()

	consensus.aggregatedCommitSig = aggSig
	consensus.commitBitmap = mask

	// If we already have a committed signature received before, check whether the new one
	// has more signatures and if yes, override the old data.
	// Otherwise, simply write the commit signature in db.
	commitSigBitmap, err := consensus.Blockchain.ReadCommitSig(blockObj.NumberU64())
	if err == nil && len(commitSigBitmap) == len(recvMsg.Payload) {
		new := mask.CountEnabled()
		mask.SetMask(commitSigBitmap[bls.BLSSignatureSizeInBytes:])
		cur := mask.CountEnabled()
		if new > cur {
			consensus.Blockchain.WriteCommitSig(blockObj.NumberU64(), recvMsg.Payload)
		}
	} else {
		consensus.Blockchain.WriteCommitSig(blockObj.NumberU64(), recvMsg.Payload)
	}

	consensus.tryCatchup()
	if recvMsg.BlockNum > consensus.blockNum {
		consensus.getLogger().Info().Uint64("MsgBlockNum", recvMsg.BlockNum).Msg("[OnCommitted] OUT OF SYNC")
		return
	}

	if consensus.IsViewChangingMode() {
		consensus.getLogger().Info().Msg("[OnCommitted] Still in ViewChanging mode, Exiting!!")
		return
	}

	if consensus.consensusTimeout[timeoutBootstrap].IsActive() {
		consensus.consensusTimeout[timeoutBootstrap].Stop()
		consensus.getLogger().Debug().Msg("[OnCommitted] Start consensus timer; stop bootstrap timer only once")
	} else {
		consensus.getLogger().Debug().Msg("[OnCommitted] Start consensus timer")
	}
	consensus.consensusTimeout[timeoutConsensus].Start()
}

// Collect private keys that are part of the current committee.
// TODO: cache valid private keys and only update when keys change.
func (consensus *Consensus) getPriKeysInCommittee() []*bls.PrivateKeyWrapper {
	priKeys := []*bls.PrivateKeyWrapper{}
	for i, key := range consensus.priKey {
		if !consensus.IsValidatorInCommittee(key.Pub.Bytes) {
			continue
		}
		priKeys = append(priKeys, &consensus.priKey[i])
	}
	return priKeys
}

func (consensus *Consensus) constructP2pMessages(msgType msg_pb.MessageType, payloadForSign []byte, priKeys []*bls.PrivateKeyWrapper) []*NetworkMessage {
	p2pMsgs := []*NetworkMessage{}
	if consensus.AggregateSig {
		networkMessage, err := consensus.construct(msgType, payloadForSign, priKeys)
		if err != nil {
			logger := consensus.getLogger().Err(err).
				Str("message-type", msgType.String())
			for _, key := range priKeys {
				logger.Str("key", key.Pri.SerializeToHexStr())
			}
			logger.Msg("could not construct message")
		} else {
			p2pMsgs = append(p2pMsgs, networkMessage)
		}

	} else {
		for _, key := range priKeys {
			networkMessage, err := consensus.construct(msgType, payloadForSign, []*bls.PrivateKeyWrapper{key})
			if err != nil {
				consensus.getLogger().Err(err).
					Str("message-type", msgType.String()).
					Str("key", key.Pri.SerializeToHexStr()).
					Msg("could not construct message")
				continue
			}

			p2pMsgs = append(p2pMsgs, networkMessage)
		}
	}
	return p2pMsgs
}

func (consensus *Consensus) broadcastConsensusP2pMessages(p2pMsgs []*NetworkMessage) error {
	groupID := []nodeconfig.GroupID{nodeconfig.NewGroupIDByShardID(nodeconfig.ShardID(consensus.ShardID))}

	for _, p2pMsg := range p2pMsgs {
		// TODO: this will not return immediately, may block
		if consensus.current.Mode() != Listening {
			if err := consensus.msgSender.SendWithoutRetry(
				groupID,
				p2p.ConstructMessage(p2pMsg.Bytes),
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (consensus *Consensus) spinUpStateSync() {
	select {
	case consensus.BlockNumLowChan <- struct{}{}:
		consensus.current.SetMode(Syncing)
		for _, v := range consensus.consensusTimeout {
			v.Stop()
		}
	default:
	}
}
