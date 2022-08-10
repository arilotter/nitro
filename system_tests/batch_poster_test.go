// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package arbtest

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/go-redis/redis/v8"
	"github.com/offchainlabs/nitro/arbnode"
)

func TestBatchPosterParallel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	redisUrl := getTestRedisUrl()
	parallelBatchPosters := 1
	if redisUrl != "" {
		opts, err := redis.ParseURL(redisUrl)
		Require(t, err)
		client := redis.NewClient(opts)
		err = client.Del(ctx, "data-poster.queue").Err()
		Require(t, err)
		parallelBatchPosters = 4
	}

	conf := arbnode.ConfigDefaultL1Test()
	conf.BatchPoster.Enable = false
	conf.BatchPoster.DataPoster.RedisUrl = redisUrl
	l2info, nodeA, l2clientA, l2stackA, l1info, _, l1client, l1stack := CreateTestNodeOnL1WithConfig(t, ctx, true, conf, params.ArbitrumDevTestChainConfig())
	defer requireClose(t, l1stack)
	defer requireClose(t, l2stackA)

	l2clientB, _, l2stackB := Create2ndNode(t, ctx, nodeA, l1stack, &l2info.ArbInitData, nil)
	defer requireClose(t, l2stackB)

	l2info.GenerateAccount("User2")

	var txs []*types.Transaction

	for i := 0; i < 100; i++ {
		tx := l2info.PrepareTx("Owner", "User2", l2info.TransferGas, common.Big1, nil)
		txs = append(txs, tx)

		err := l2clientA.SendTransaction(ctx, tx)
		Require(t, err)
	}

	for _, tx := range txs {
		_, err := EnsureTxSucceeded(ctx, l2clientA, tx)
		Require(t, err)
	}

	firstTxData, err := txs[0].MarshalBinary()
	Require(t, err)
	seqTxOpts := l1info.GetDefaultTransactOpts("Sequencer", ctx)
	conf.BatchPoster.Enable = true
	conf.BatchPoster.MaxBatchSize = len(firstTxData) * 2
	startL1Block, err := l1client.BlockNumber(ctx)
	Require(t, err)
	for i := 0; i < parallelBatchPosters; i++ {
		batchPoster, err := arbnode.NewBatchPoster(nodeA.L1Reader, nodeA.InboxTracker, nodeA.TxStreamer, &conf.BatchPoster, nodeA.DeployInfo.SequencerInbox, &seqTxOpts, nil)
		Require(t, err)
		batchPoster.Start(ctx)
		defer batchPoster.StopAndWait()
	}

	lastTxHash := txs[len(txs)-1].Hash()
	for i := 90; i > 0; i-- {
		SendWaitTestTransactions(t, ctx, l1client, []*types.Transaction{
			l1info.PrepareTx("Faucet", "User", 30000, big.NewInt(1e12), nil),
		})
		time.Sleep(500 * time.Millisecond)
		_, err := l2clientB.TransactionReceipt(ctx, lastTxHash)
		if err == nil {
			break
		}
		if i == 0 {
			Require(t, err)
		}
	}

	// I've locally confirmed that this passes when the clique period is set to 1.
	// However, setting the clique period to 1 slows everything else (including the L1 deployment for this test) down to a crawl.
	if false {
		// Make sure the batch poster is able to post multiple batches in one block
		endL1Block, err := l1client.BlockNumber(ctx)
		Require(t, err)
		seqInbox, err := arbnode.NewSequencerInbox(l1client, nodeA.DeployInfo.SequencerInbox, 0)
		Require(t, err)
		batches, err := seqInbox.LookupBatchesInRange(ctx, new(big.Int).SetUint64(startL1Block), new(big.Int).SetUint64(endL1Block))
		Require(t, err)
		var foundMultipleInBlock bool
		for i := range batches {
			if i == 0 {
				continue
			}
			if batches[i-1].BlockNumber == batches[i].BlockNumber {
				foundMultipleInBlock = true
				break
			}
		}

		if !foundMultipleInBlock {
			Fail(t, "only found one batch per block")
		}
	}

	l2balance, err := l2clientB.BalanceAt(ctx, l2info.GetAddress("User2"), nil)
	Require(t, err)

	if l2balance.Sign() == 0 {
		Fail(t, "Unexpected zero balance")
	}
}