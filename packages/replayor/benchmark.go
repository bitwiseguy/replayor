package replayor

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/danyalprout/replayor/packages/clients"
	"github.com/danyalprout/replayor/packages/stats"
	"github.com/danyalprout/replayor/packages/strategies"
	"github.com/ethereum-optimism/optimism/op-bindings/predeploys"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/retry"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	concurrency = 25
)

type Benchmark struct {
	clients clients.Clients
	s       stats.Stats

	log log.Logger

	currentBlock *types.Block

	incomingBlocks chan *types.Block
	processBlocks  chan strategies.BlockCreationParams

	previousReplayedBlockHash common.Hash
	strategy                  strategies.Strategy

	sm                  sync.Mutex
	remainingBlockCount uint64
	rollupCfg           *rollup.Config
	startBlockNum       uint64
	endBlockNum         uint64

	benchmarkOpcodes bool
	diffStorage      bool
}

func (r *Benchmark) getBlockFromSourceNode(ctx context.Context, blockNum uint64) (*types.Block, error) {
	return retry.Do(ctx, 10, retry.Exponential(), func() (*types.Block, error) {
		return r.clients.SourceNode.BlockByNumber(ctx, big.NewInt(int64(blockNum)))
	})
}

func (r *Benchmark) loadBlocks(ctx context.Context) {
	for blockStartRange := r.startBlockNum; blockStartRange <= r.endBlockNum; blockStartRange += concurrency {
		results := make([]*types.Block, concurrency)

		var wg sync.WaitGroup
		wg.Add(concurrency)

		var m sync.Mutex

		for i := uint64(0); i < concurrency; i++ {
			blockNum := blockStartRange + i

			go func(index, blockNum uint64) {
				defer wg.Done()

				block, err := r.getBlockFromSourceNode(ctx, blockNum)
				if err != nil {
					panic(err)
				}

				if block == nil {
					panic(fmt.Errorf("unexpected nil block: %d", blockNum))
				}

				m.Lock()
				results[index] = block
				m.Unlock()
			}(i, blockNum)
		}

		wg.Wait()

		for _, block := range results {
			r.incomingBlocks <- block
		}
	}

	r.log.Info("finished loading blocks, closing channel")
	close(r.incomingBlocks)
}

func (r *Benchmark) addBlock(ctx context.Context, currentBlock strategies.BlockCreationParams) {
	l := r.log.New("source", "add-block", "block", currentBlock.Number)

	stats := stats.BlockCreationStats{}

	txns := currentBlock.Transactions

	stats.TxnCount = len(txns)

	state := &eth.ForkchoiceState{
		HeadBlockHash:      r.previousReplayedBlockHash,
		SafeBlockHash:      r.previousReplayedBlockHash,
		FinalizedBlockHash: r.previousReplayedBlockHash,
	}

	txnData := make([]eth.Data, len(txns))
	for i, txn := range txns {
		data, err := txn.MarshalBinary()
		if err != nil {
			panic(err)
		}
		txnData[i] = data
		stats.GasLimit += txn.Gas()
	}

	attrs := &eth.PayloadAttributes{
		Timestamp:             currentBlock.Time,
		NoTxPool:              true,
		SuggestedFeeRecipient: predeploys.SequencerFeeVaultAddr,
		Transactions:          txnData,
		GasLimit:              currentBlock.GasLimit,
		PrevRandao:            currentBlock.MixDigest,
		ParentBeaconBlockRoot: currentBlock.BeaconRoot,
	}

	if r.rollupCfg.IsCanyon(uint64(currentBlock.Time)) {
		attrs.Withdrawals = &types.Withdrawals{}
	}

	startTime := time.Now()

	result, err := r.clients.EngineApi.ForkchoiceUpdate(ctx, state, attrs)

	fcuEnd := time.Now()

	if err != nil {
		l.Crit("forkchoice update failed", "err", err)
	}

	if result.PayloadStatus.Status != eth.ExecutionValid {
		l.Crit("forkchoice update failed", "status", result.PayloadStatus.Status)
	}

	stats.FCUTime = fcuEnd.Sub(startTime)

	envelope, err := r.clients.EngineApi.GetPayload(ctx, eth.PayloadInfo{
		ID:        *result.PayloadID,
		Timestamp: uint64(currentBlock.Time),
	})

	if err != nil {
		l.Crit("get payload failed", "err", err, "payloadId", *result.PayloadID)
	}

	getTime := time.Now()

	stats.GetTime = getTime.Sub(fcuEnd)

	err = r.strategy.ValidateExecution(ctx, envelope, currentBlock)
	if err != nil {
		txnHash := make([]common.Hash, len(txns))
		for i, txn := range txns {
			txnHash[i] = txn.Hash()
		}

		l.Crit("validation failed", "err", err, "executionPayload", *envelope.ExecutionPayload, "parentBeaconBlockRoot", envelope.ParentBeaconBlockRoot, "txnHashes", txnHash)
	}

	status, err := r.clients.EngineApi.NewPayload(ctx, envelope.ExecutionPayload, envelope.ParentBeaconBlockRoot)
	if err != nil {
		l.Crit("new payload failed", "err", err)
	}

	newEnd := time.Now()
	stats.NewTime = newEnd.Sub(getTime)

	if status.Status != eth.ExecutionValid {
		l.Crit("new payload failed", "status", status.Status)
	}

	state = &eth.ForkchoiceState{
		HeadBlockHash:      envelope.ExecutionPayload.BlockHash,
		SafeBlockHash:      envelope.ExecutionPayload.BlockHash,
		FinalizedBlockHash: envelope.ExecutionPayload.BlockHash,
	}

	status2, err := r.clients.EngineApi.ForkchoiceUpdate(ctx, state, nil)
	if err != nil {
		l.Crit("forkchoice update failed", "err", err)
	}

	if status2.PayloadStatus.Status != eth.ExecutionValid {
		l.Crit("forkchoice update failed", "status", status2.PayloadStatus.Status)
	}

	err = r.strategy.ValidateBlock(ctx, envelope, currentBlock)
	if err != nil {
		l.Crit("validation failed", "err", err)
	}

	fcu2Time := time.Now()

	stats.FCUNoAttrsTime = fcu2Time.Sub(newEnd)
	stats.TotalTime = fcu2Time.Sub(startTime)
	stats.BlockNumber = uint64(envelope.ExecutionPayload.BlockNumber)
	stats.BlockHash = envelope.ExecutionPayload.BlockHash

	r.previousReplayedBlockHash = envelope.ExecutionPayload.BlockHash

	r.enrich(ctx, &stats)
	r.s.RecordBlockStats(stats)
}

func (r *Benchmark) enrich(ctx context.Context, s *stats.BlockCreationStats) {
	receipts, err := retry.Do(ctx, 3, retry.Exponential(), func() ([]*types.Receipt, error) {
		return r.clients.DestNode.BlockReceipts(ctx, rpc.BlockNumberOrHash{BlockHash: &s.BlockHash})
	})

	if err != nil {
		r.log.Warn("unable to load receipts", "err", err)
	}

	success := 0
	for _, receipt := range receipts {
		if receipt.Status == types.ReceiptStatusSuccessful {
			success += 1
			s.GasUsed += receipt.GasUsed
		}
	}
	s.Success = float64(success) / float64(len(receipts))

	r.computeTraceStats(ctx, s, receipts)
}

func (r *Benchmark) submitBlocks(ctx context.Context) {
	for {
		select {
		case block, ok := <-r.processBlocks:
			if block.Number > r.endBlockNum || !ok {
				r.log.Info("stopping block processing")
				return
			}

			r.addBlock(ctx, block)
			r.remainingBlockCount -= 1
		case <-ctx.Done():
			return
		}
	}
}

func (r *Benchmark) mapBlocks(ctx context.Context) {
	for {
		select {
		case b, ok := <-r.incomingBlocks:
			if !ok {
				r.log.Info("stopping block mapping")
				close(r.processBlocks)
				return
			} else if b == nil {
				r.log.Warn("nil block received")
				continue
			}

			params := r.strategy.BlockReceived(ctx, b)
			if params == nil {
				continue
			}

			r.processBlocks <- *params
		case <-ctx.Done():
			return
		}
	}

}

func (r *Benchmark) Run(ctx context.Context) {
	doneChan := make(chan any)
	go r.loadBlocks(ctx)
	go r.mapBlocks(ctx)
	go func() {
		r.submitBlocks(ctx)
		close(doneChan)
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	l := r.log.New("source", "monitor")

	lastBlockNum := r.currentBlock.NumberU64()

	for {
		select {
		case <-doneChan:
			r.log.Info("writing block stats")
			r.s.Write(ctx)
			return
		case <-ticker.C:
			currentBlock, err := r.clients.DestNode.BlockByNumber(ctx, nil)
			if err != nil {
				l.Error("unable to load current block", "err", err)
			}

			l.Info("replay progress", "blocks", currentBlock.NumberU64()-lastBlockNum, "incomingBlocks", len(r.incomingBlocks), "processBlocks", len(r.processBlocks), "currentBlock", currentBlock.NumberU64(), "remaining", r.remainingBlockCount)

			lastBlockNum = currentBlock.NumberU64()
		case <-ctx.Done():
			return
		}
	}
}

// Start block
// End block
func NewBenchmark(
	c clients.Clients,
	rollupCfg *rollup.Config,
	logger log.Logger,
	strategy strategies.Strategy,
	s stats.Stats,
	currentBlock *types.Block,
	benchmarkBlockCount uint64,
	benchmarkOpcodes bool,
	diffStorage bool) *Benchmark {
	r := &Benchmark{
		clients:                   c,
		rollupCfg:                 rollupCfg,
		log:                       logger,
		incomingBlocks:            make(chan *types.Block, 25),
		processBlocks:             make(chan strategies.BlockCreationParams, 25),
		strategy:                  strategy,
		s:                         s,
		currentBlock:              currentBlock,
		startBlockNum:             currentBlock.NumberU64() + 1,
		endBlockNum:               currentBlock.NumberU64() + benchmarkBlockCount,
		remainingBlockCount:       benchmarkBlockCount,
		previousReplayedBlockHash: currentBlock.Hash(),
		benchmarkOpcodes:          benchmarkOpcodes,
		diffStorage:               diffStorage,
	}

	return r
}
