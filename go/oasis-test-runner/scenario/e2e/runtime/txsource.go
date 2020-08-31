package runtime

import (
	"context"
	"crypto"
	cryptoRand "crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oasisprotocol/oasis-core/go/common/crypto/drbg"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/mathrand"
	commonGrpc "github.com/oasisprotocol/oasis-core/go/common/grpc"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/quantity"
	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
	"github.com/oasisprotocol/oasis-core/go/consensus/api/transaction"
	"github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/common"
	"github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/common/flags"
	"github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/debug/txsource"
	"github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/debug/txsource/workload"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/env"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/log"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/oasis"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/scenario"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/scenario/e2e"
	staking "github.com/oasisprotocol/oasis-core/go/staking/api"
	"github.com/oasisprotocol/oasis-core/go/storage/database"
)

const (
	timeLimitShort = 3 * time.Minute
	timeLimitLong  = 12 * time.Hour

	nodeRestartIntervalLong = 2 * time.Minute
	nodeLongRestartInterval = 15 * time.Minute
	nodeLongRestartDuration = 10 * time.Minute
	livenessCheckInterval   = 1 * time.Minute
	txSourceGasPrice        = 1
)

// TxSourceMultiShort uses multiple workloads for a short time.
var TxSourceMultiShort scenario.Scenario = &txSourceImpl{
	runtimeImpl: *newRuntimeImpl("txsource-multi-short", "", nil),
	clientWorkloads: []string{
		workload.NameCommission,
		workload.NameDelegation,
		workload.NameOversized,
		workload.NameParallel,
		workload.NameRegistration,
		workload.NameRuntime,
		workload.NameTransfer,
	},
	allNodeWorkloads: []string{
		workload.NameQueries,
	},
	timeLimit:                         timeLimitShort,
	livenessCheckInterval:             livenessCheckInterval,
	consensusPruneDisabledProbability: 0.1,
	consensusPruneMinKept:             100,
	consensusPruneMaxKept:             200,
	// XXX: use 2 storage nodes as SGX E2E test instances cannot handle any
	// more nodes that are currently configured, and runtime requires 2 nodes.
	numStorageNodes: 2,
}

// TxSourceMulti uses multiple workloads.
var TxSourceMulti scenario.Scenario = &txSourceImpl{
	runtimeImpl: *newRuntimeImpl("txsource-multi", "", nil),
	clientWorkloads: []string{
		workload.NameCommission,
		workload.NameDelegation,
		workload.NameOversized,
		workload.NameParallel,
		workload.NameRegistration,
		workload.NameRuntime,
		workload.NameTransfer,
	},
	allNodeWorkloads: []string{
		workload.NameQueries,
	},
	timeLimit:                         timeLimitLong,
	nodeRestartInterval:               nodeRestartIntervalLong,
	nodeLongRestartInterval:           nodeLongRestartInterval,
	nodeLongRestartDuration:           nodeLongRestartDuration,
	livenessCheckInterval:             livenessCheckInterval,
	consensusPruneDisabledProbability: 0.1,
	consensusPruneMinKept:             100,
	consensusPruneMaxKept:             1000,
	// Nodes getting killed commonly result in corrupted tendermint WAL when the
	// node is restarted. Enable automatic corrupted WAL recovery for validator
	// nodes.
	tendermintRecoverCorruptedWAL: true,
	// Use 3 storage nodes so runtime continues to work when one of the nodes
	// is shut down.
	numStorageNodes: 3,
}

type txSourceImpl struct { // nolint: maligned
	runtimeImpl

	clientWorkloads  []string
	allNodeWorkloads []string

	timeLimit               time.Duration
	nodeRestartInterval     time.Duration
	nodeLongRestartInterval time.Duration
	nodeLongRestartDuration time.Duration
	livenessCheckInterval   time.Duration

	consensusPruneDisabledProbability float32
	consensusPruneMinKept             int64
	consensusPruneMaxKept             int64

	tendermintRecoverCorruptedWAL bool

	// Configurable number of storage nodes. If running tests with long node
	// shutdowns enabled, make sure this is at least `MinWriteReplication+1`,
	// so that the runtime continues to work, even if one of the nodes is shut
	// down.
	// XXX: this is configurable because SGX E2E test instances cannot handle
	// more test nodes that we already use, and we don't need additional storage
	// nodes in the short test variant.
	numStorageNodes int

	rng  *rand.Rand
	seed string
}

func (sc *txSourceImpl) PreInit(childEnv *env.Env) error {
	// Generate a new random seed and log it so we can reproduce the run.
	// Use existing seed, if it already exists.
	if sc.seed == "" {
		rawSeed := make([]byte, 16)
		_, err := cryptoRand.Read(rawSeed)
		if err != nil {
			return fmt.Errorf("failed to generate random seed: %w", err)
		}
		sc.seed = hex.EncodeToString(rawSeed)

		sc.Logger.Info("using random seed",
			"seed", sc.seed,
		)
	}

	// Set up the deterministic random source.
	hash := crypto.SHA512
	src, err := drbg.New(hash, []byte(sc.seed), nil, []byte("txsource scenario"))
	if err != nil {
		return fmt.Errorf("failed to create random source: %w", err)
	}
	sc.rng = rand.New(mathrand.New(src))

	return nil
}

func (sc *txSourceImpl) generateConsensusFixture(f *oasis.ConsensusFixture, forceDisableConsensusPrune bool) {
	// Randomize pruning configuration.
	p := sc.rng.Float32()
	switch {
	case forceDisableConsensusPrune || p < sc.consensusPruneDisabledProbability:
		f.PruneNumKept = 0
	default:
		// [sc.consensusPruneMinKept, sc.consensusPruneMaxKept]
		f.PruneNumKept = uint64(sc.rng.Int63n(sc.consensusPruneMaxKept-sc.consensusPruneMinKept+1) + sc.consensusPruneMinKept)
	}
}

func (sc *txSourceImpl) Fixture() (*oasis.NetworkFixture, error) {
	f, err := sc.runtimeImpl.Fixture()
	if err != nil {
		return nil, err
	}
	// Use deterministic identities as we need to allocate funds to nodes.
	f.Network.DeterministicIdentities = true
	f.Network.StakingGenesis = &staking.Genesis{
		Parameters: staking.ConsensusParameters{
			CommissionScheduleRules: staking.CommissionScheduleRules{
				RateChangeInterval: 10,
				RateBoundLead:      30,
				MaxRateSteps:       12,
				MaxBoundSteps:      12,
			},
			DebondingInterval: 2,
			GasCosts: transaction.Costs{
				staking.GasOpTransfer:      10,
				staking.GasOpBurn:          10,
				staking.GasOpAddEscrow:     10,
				staking.GasOpReclaimEscrow: 10,
			},
			FeeSplitWeightPropose:     *quantity.NewFromUint64(2),
			FeeSplitWeightVote:        *quantity.NewFromUint64(1),
			FeeSplitWeightNextPropose: *quantity.NewFromUint64(1),
		},
		TotalSupply: *quantity.NewFromUint64(130000000000),
		Ledger: map[staking.Address]*staking.Account{
			e2e.DeterministicValidator0: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicValidator1: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicValidator2: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicValidator3: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicCompute0: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicCompute1: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicCompute2: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicCompute3: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicStorage0: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicStorage1: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicStorage2: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicKeyManager0: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
			e2e.DeterministicKeyManager1: {
				General: staking.GeneralAccount{
					Balance: *quantity.NewFromUint64(10000000000),
				},
			},
		},
	}

	if sc.nodeRestartInterval > 0 {
		// If node restarts enabled, do not enable round timeouts, failures or
		// discrepancy log watchers.
		f.Network.DefaultLogWatcherHandlerFactories = []log.WatcherHandlerFactory{}
	}

	// Disable CheckTx on the client node so we can submit invalid transactions.
	f.Clients[0].Consensus.DisableCheckTx = true

	// Set up checkpointing.
	f.Runtimes[1].Storage.CheckpointInterval = 1000
	f.Runtimes[1].Storage.CheckpointNumKept = 2
	f.Runtimes[1].Storage.CheckpointChunkSize = 1024 * 1024

	// Use at least 4 validators so that consensus can keep making progress
	// when a node is being killed and restarted.
	f.Validators = []oasis.ValidatorFixture{
		{Entity: 1},
		{Entity: 1},
		{Entity: 1},
		{Entity: 1},
	}
	f.ComputeWorkers = []oasis.ComputeWorkerFixture{
		{Entity: 1, Runtimes: []int{1}},
		{Entity: 1, Runtimes: []int{1}},
		{Entity: 1, Runtimes: []int{1}},
		{Entity: 1, Runtimes: []int{1}},
	}
	f.Keymanagers = []oasis.KeymanagerFixture{
		{Runtime: 0, Entity: 1},
		{Runtime: 0, Entity: 1},
	}
	var storageWorkers []oasis.StorageWorkerFixture
	for i := 0; i < sc.numStorageNodes; i++ {
		storageWorkers = append(storageWorkers, oasis.StorageWorkerFixture{
			Backend: database.BackendNameBadgerDB,
			Entity:  1,
		})
	}
	f.StorageWorkers = storageWorkers

	// Update validators to require fee payments.
	for i := range f.Validators {
		f.Validators[i].Consensus.MinGasPrice = txSourceGasPrice
		f.Validators[i].Consensus.SubmissionGasPrice = txSourceGasPrice
		f.Validators[i].Consensus.TendermintRecoverCorruptedWAL = sc.tendermintRecoverCorruptedWAL
		// Ensure validator-0 does not have pruning enabled, so nodes taken down
		// for long period can sync from it.
		// Note: validator-0 is also never restarted.
		sc.generateConsensusFixture(&f.Validators[i].Consensus, i == 0)
	}
	// Update all other nodes to use a specific gas price.
	for i := range f.Keymanagers {
		f.Keymanagers[i].Consensus.SubmissionGasPrice = txSourceGasPrice
		sc.generateConsensusFixture(&f.Keymanagers[i].Consensus, false)
	}
	for i := range f.StorageWorkers {
		f.StorageWorkers[i].Consensus.SubmissionGasPrice = txSourceGasPrice
		sc.generateConsensusFixture(&f.StorageWorkers[i].Consensus, false)
		if i > 0 {
			f.StorageWorkers[i].CheckpointSyncEnabled = true
		}
	}
	for i := range f.ComputeWorkers {
		f.ComputeWorkers[i].Consensus.SubmissionGasPrice = txSourceGasPrice
		sc.generateConsensusFixture(&f.ComputeWorkers[i].Consensus, false)
	}
	for i := range f.ByzantineNodes {
		f.ByzantineNodes[i].Consensus.SubmissionGasPrice = txSourceGasPrice
		sc.generateConsensusFixture(&f.ByzantineNodes[i].Consensus, false)
	}

	return f, nil
}

func (sc *txSourceImpl) manager(env *env.Env, errCh chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	// Make sure we exit when the environment gets torn down.
	stopCh := make(chan struct{})
	env.AddOnCleanup(func() {
		cancel()
		close(stopCh)
	})

	if sc.nodeRestartInterval > 0 {
		sc.Logger.Info("random node restarts enabled",
			"restart_interval", sc.nodeRestartInterval,
		)
	} else {
		sc.nodeRestartInterval = math.MaxInt64
	}
	if sc.nodeLongRestartInterval > 0 {
		sc.Logger.Info("random long node restarts enabled",
			"interval", sc.nodeLongRestartInterval,
			"start_delay", sc.nodeLongRestartDuration,
		)
	} else {
		sc.nodeLongRestartInterval = math.MaxInt64
	}

	// Setup restarable nodes.
	var restartableLock sync.Mutex
	var longRestartNode *oasis.Node
	var restartableNodes []*oasis.Node
	// Keep one of each types of nodes always running.
	for _, v := range sc.Net.Validators()[1:] {
		restartableNodes = append(restartableNodes, &v.Node)
	}
	for _, s := range sc.Net.StorageWorkers()[1:] {
		restartableNodes = append(restartableNodes, &s.Node)
	}
	for _, c := range sc.Net.ComputeWorkers()[1:] {
		restartableNodes = append(restartableNodes, &c.Node)
	}
	for _, k := range sc.Net.Keymanagers()[1:] {
		restartableNodes = append(restartableNodes, &k.Node)
	}

	restartTicker := time.NewTicker(sc.nodeRestartInterval)
	defer restartTicker.Stop()

	livenessTicker := time.NewTicker(sc.livenessCheckInterval)
	defer livenessTicker.Stop()

	longRestartTicker := time.NewTicker(sc.nodeLongRestartInterval)
	defer longRestartTicker.Stop()

	var nodeIndex int
	var lastHeight int64
	for {
		select {
		case <-stopCh:
			return
		case <-restartTicker.C:
			func() {
				restartableLock.Lock()
				defer restartableLock.Unlock()

				// Reshuffle nodes each time the counter wraps around.
				if nodeIndex == 0 {
					sc.rng.Shuffle(len(restartableNodes), func(i, j int) {
						restartableNodes[i], restartableNodes[j] = restartableNodes[j], restartableNodes[i]
					})
				}
				// Ensure the current node is not being restarted already.
				if longRestartNode != nil && restartableNodes[nodeIndex].NodeID.Equal(longRestartNode.NodeID) {
					nodeIndex = (nodeIndex + 1) % len(restartableNodes)
				}

				// Choose a random node and restart it.
				node := restartableNodes[nodeIndex]
				sc.Logger.Info("restarting node",
					"node", node.Name,
				)
				if err := node.Restart(ctx); err != nil {
					sc.Logger.Error("failed to restart node",
						"node", node.Name,
						"err", err,
					)
					errCh <- err
					return
				}
				sc.Logger.Info("node restarted",
					"node", node.Name,
				)
				nodeIndex = (nodeIndex + 1) % len(restartableNodes)
			}()
		case <-longRestartTicker.C:
			// Choose a random node and restart it.
			restartableLock.Lock()
			if longRestartNode != nil {
				sc.Logger.Info("node already stopped, skipping",
					"node", longRestartNode,
				)
				restartableLock.Unlock()
				continue
			}

			longRestartNode = restartableNodes[sc.rng.Intn(len(restartableNodes))]
			selectedNode := longRestartNode
			restartableLock.Unlock()
			go func() {
				sc.Logger.Info("stopping node",
					"node", selectedNode.Name,
					"start_delay", sc.nodeLongRestartDuration,
				)
				if err := selectedNode.RestartAfter(ctx, sc.nodeLongRestartDuration); err != nil {
					sc.Logger.Error("failed to restart node",
						"node", selectedNode.Name,
						"err", err,
					)
					errCh <- err
					return
				}
				sc.Logger.Info("starting node",
					"node", selectedNode.Name,
					"start_delay", sc.nodeLongRestartDuration,
				)

				restartableLock.Lock()
				longRestartNode = nil
				restartableLock.Unlock()
			}()

		case <-livenessTicker.C:
			// Check if consensus has made any progress.
			livenessCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			blk, err := sc.Net.Controller().Consensus.GetBlock(livenessCtx, consensus.HeightLatest)
			cancel()
			if err != nil {
				sc.Logger.Warn("failed to query latest consensus block",
					"err", err,
				)
				continue
			}

			if blk.Height <= lastHeight {
				sc.Logger.Error("consensus hasn't made any progress since last liveness check",
					"last_height", lastHeight,
					"height", blk.Height,
				)
				errCh <- fmt.Errorf("consensus is dead")
				return
			}

			sc.Logger.Info("current consensus height",
				"height", blk.Height,
			)
			lastHeight = blk.Height
		}
	}
}

func (sc *txSourceImpl) startWorkload(childEnv *env.Env, errCh chan error, name string, node *oasis.Node) error {
	sc.Logger.Info("starting workload",
		"name", name,
		"node", node.Name,
	)

	d, err := childEnv.NewSubDir(fmt.Sprintf("workload-%s", name))
	if err != nil {
		return err
	}
	d, err = d.NewSubDir(node.Name)
	if err != nil {
		return err
	}

	w, err := d.NewLogWriter(fmt.Sprintf("workload-%s.log", name))
	if err != nil {
		return err
	}

	logFmt := logging.FmtJSON
	logLevel := logging.LevelDebug

	args := []string{
		"debug", "txsource",
		"--address", "unix:" + node.SocketPath(),
		"--" + common.CfgDebugAllowTestKeys,
		"--" + common.CfgDataDir, d.String(),
		"--" + flags.CfgDebugDontBlameOasis,
		"--" + flags.CfgDebugTestEntity,
		"--log.format", logFmt.String(),
		"--log.level", logLevel.String(),
		"--" + commonGrpc.CfgLogDebug,
		"--" + flags.CfgGenesisFile, sc.Net.GenesisPath(),
		"--" + workload.CfgRuntimeID, runtimeID.String(),
		"--" + txsource.CfgWorkload, name,
		"--" + txsource.CfgTimeLimit, sc.timeLimit.String(),
		"--" + txsource.CfgSeed, sc.seed,
		// Use half the configured interval due to fast blocks.
		"--" + workload.CfgConsensusNumKeptVersions, strconv.FormatUint(node.Consensus().PruneNumKept/2, 10),
	}
	// Disable runtime queries on non-client node.
	if node.Name != sc.Net.Clients()[0].Name {
		args = append(args, "--"+workload.CfgQueriesRuntimeEnabled+"=false")
	}
	nodeBinary := sc.Net.Config().NodeBinary

	cmd := exec.Command(nodeBinary, args...)
	cmd.SysProcAttr = env.CmdAttrs
	cmd.Stdout = w
	cmd.Stderr = w

	// Setup verbose http2 requests logging for nodes. Investigating EOF gRPC
	// failures.
	if name == workload.NameQueries {
		cmd.Env = append(os.Environ(),
			"GODEBUG=http2debug=1",
		)
	}

	sc.Logger.Info("launching workload binary",
		"args", strings.Join(args, " "),
	)

	if err = cmd.Start(); err != nil {
		return err
	}

	go func() {
		errCh <- cmd.Wait()

		sc.Logger.Info("workload finished",
			"name", name,
			"node", node.Name,
		)
	}()

	return nil
}

func (sc *txSourceImpl) Clone() scenario.Scenario {
	return &txSourceImpl{
		runtimeImpl:                       *sc.runtimeImpl.Clone().(*runtimeImpl),
		clientWorkloads:                   sc.clientWorkloads,
		allNodeWorkloads:                  sc.allNodeWorkloads,
		timeLimit:                         sc.timeLimit,
		nodeRestartInterval:               sc.nodeRestartInterval,
		nodeLongRestartDuration:           sc.nodeLongRestartDuration,
		nodeLongRestartInterval:           sc.nodeLongRestartInterval,
		livenessCheckInterval:             sc.livenessCheckInterval,
		consensusPruneDisabledProbability: sc.consensusPruneDisabledProbability,
		consensusPruneMinKept:             sc.consensusPruneMinKept,
		consensusPruneMaxKept:             sc.consensusPruneMaxKept,
		tendermintRecoverCorruptedWAL:     sc.tendermintRecoverCorruptedWAL,
		numStorageNodes:                   sc.numStorageNodes,
		seed:                              sc.seed,
		// rng must always be reinitialized from seed by calling PreInit().
	}
}

func (sc *txSourceImpl) Run(childEnv *env.Env) error {
	if err := sc.Net.Start(); err != nil {
		return fmt.Errorf("scenario net Start: %w", err)
	}

	// Wait for all nodes to be synced before we proceed.
	if err := sc.waitNodesSynced(); err != nil {
		return err
	}

	ctx := context.Background()

	sc.Logger.Info("waiting for network to come up")
	if err := sc.Net.Controller().WaitNodesRegistered(ctx, sc.Net.NumRegisterNodes()); err != nil {
		return fmt.Errorf("WaitNodesRegistered: %w", err)
	}

	// Start all configured workloads.
	errCh := make(chan error, len(sc.clientWorkloads)+len(sc.allNodeWorkloads)+2)
	for _, name := range sc.clientWorkloads {
		if err := sc.startWorkload(childEnv, errCh, name, &sc.Net.Clients()[0].Node); err != nil {
			return fmt.Errorf("failed to start client workload %s: %w", name, err)
		}
	}
	nodes := sc.Net.Nodes()
	for _, name := range sc.allNodeWorkloads {
		for _, node := range nodes {
			if err := sc.startWorkload(childEnv, errCh, name, node); err != nil {
				return fmt.Errorf("failed to start workload %s on node %s: %w", name, node.Name, err)
			}
		}
	}
	// Start background scenario manager.
	go sc.manager(childEnv, errCh)

	// Wait for any workload to terminate.
	var err error
	select {
	case err = <-sc.Net.Errors():
	case err = <-errCh:
	}
	if err != nil {
		return err
	}

	if err = sc.Net.CheckLogWatchers(); err != nil {
		return err
	}

	return nil
}
