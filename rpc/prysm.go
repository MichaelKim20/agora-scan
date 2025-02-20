package rpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"eth2-exporter/db"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"fmt"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	"math/big"
	"net/http"
	"sort"
	"sync"
	"time"

	gtypes "github.com/ethereum/go-ethereum/core/types"

	lru "github.com/hashicorp/golang-lru"
	ethpb "github.com/prysmaticlabs/prysm/v4/proto/prysm/v1alpha1"

	"github.com/prysmaticlabs/go-bitfield"
	"google.golang.org/grpc"

	"github.com/golang/protobuf/ptypes/empty"
	eth2types "github.com/prysmaticlabs/prysm/v4/consensus-types/primitives"
)

// PrysmClient holds information about the Prysm Client
type PrysmClient struct {
	client              ethpb.BeaconChainClient
	endpoint            string
	nodeClient          ethpb.NodeClient
	conn                *grpc.ClientConn
	assignmentsCache    *lru.Cache
	assignmentsCacheMux *sync.Mutex
	newBlockChan        chan *types.Block
	signer              gtypes.Signer
}

var PrysmLatestHeadSlot uint64 = 0

// NewPrysmClient is used for a new Prysm client connection
func NewPrysmClient(grpcEndpoint string, rpcEndpoint string, chainId *big.Int) (*PrysmClient, error) {
	dialOpts := []grpc.DialOption{
		grpc.WithInsecure(),
		// Maximum receive value 128 MB
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(128 * 1024 * 1024)),
	}
	conn, err := grpc.Dial(grpcEndpoint, dialOpts...)

	if err != nil {
		return nil, err
	}

	chainClient := ethpb.NewBeaconChainClient(conn)
	nodeClient := ethpb.NewNodeClient(conn)

	logger.Printf("gRPC connection to backend node established")
	client := &PrysmClient{
		client:              chainClient,
		endpoint:            rpcEndpoint,
		nodeClient:          nodeClient,
		conn:                conn,
		assignmentsCacheMux: &sync.Mutex{},
		newBlockChan:        make(chan *types.Block, 1000),
		signer:              gtypes.NewLondonSigner(chainId),
	}
	client.assignmentsCache, _ = lru.New(10)

	streamChainHeadClient, err := chainClient.StreamChainHead(context.Background(), &empty.Empty{})
	if err != nil {
		return nil, err
	}

	go func() {
		for {
			head, err := streamChainHeadClient.Recv()

			if err != nil {
				logger.Errorf("error receiving from chain head stream: %v", err)

				// in order to recover from a stream error we wait for a second and then re-create the stream
				time.Sleep(time.Second)
				streamChainHeadClient, err = chainClient.StreamChainHead(context.Background(), &empty.Empty{})
				for err != nil {
					logger.Errorf("error initializing chain head stream: %v. retrying in 1s...", err)
					time.Sleep(time.Second)
					streamChainHeadClient, err = chainClient.StreamChainHead(context.Background(), &empty.Empty{})
				}
				continue
			}

			blocks, err := client.GetBlocksBySlot(uint64(head.HeadSlot))

			if err != nil {
				logger.Errorf("error receiving blocks via chain head stream: %v", err)
				continue
			}

			for _, b := range blocks {
				logger.Infof("received block at slot %v with hash %x via stream", blocks[0].Slot, blocks[0].BlockRoot)
				client.newBlockChan <- b
			}
		}
	}()
	return client, nil
}

// Close will close a Prysm client connection
func (pc *PrysmClient) Close() {
	pc.conn.Close()
}

func (pc *PrysmClient) GetNewBlockChan() chan *types.Block {
	return pc.newBlockChan
}

// GetGenesisTimestamp returns the genesis timestamp of the beacon chain
func (pc *PrysmClient) GetGenesisTimestamp() (int64, error) {
	genesis, err := pc.nodeClient.GetGenesis(context.Background(), &empty.Empty{})

	if err != nil {
		return 0, err
	}

	return genesis.GenesisTime.Seconds, nil
}

// GetChainHead will get the chain head from a Prysm client
func (pc *PrysmClient) GetChainHead() (*types.ChainHead, error) {
	headResponse, err := pc.client.GetChainHead(context.Background(), &empty.Empty{})

	if err != nil {
		return nil, err
	}

	return &types.ChainHead{
		HeadSlot:                   uint64(headResponse.HeadSlot),
		HeadEpoch:                  uint64(headResponse.HeadEpoch),
		HeadBlockRoot:              headResponse.HeadBlockRoot,
		FinalizedSlot:              uint64(headResponse.FinalizedSlot),
		FinalizedEpoch:             uint64(headResponse.FinalizedEpoch),
		FinalizedBlockRoot:         headResponse.FinalizedBlockRoot,
		JustifiedSlot:              uint64(headResponse.JustifiedSlot),
		JustifiedEpoch:             uint64(headResponse.JustifiedEpoch),
		JustifiedBlockRoot:         headResponse.JustifiedBlockRoot,
		PreviousJustifiedSlot:      uint64(headResponse.PreviousJustifiedSlot),
		PreviousJustifiedEpoch:     uint64(headResponse.PreviousJustifiedEpoch),
		PreviousJustifiedBlockRoot: headResponse.PreviousJustifiedBlockRoot,
	}, nil
}

// GetValidatorQueue will get the validator queue from a Prysm client
func (pc *PrysmClient) GetValidatorQueue() (*types.ValidatorQueue, error) {
	var err error

	validators, err := pc.client.GetValidatorQueue(context.Background(), &empty.Empty{})

	if err != nil {
		return nil, fmt.Errorf("error retrieving validator queue data: %v", err)
	}

	return &types.ValidatorQueue{
		Activating: uint64(len(validators.GetActivationValidatorIndices())),
		Exititing:  uint64(len(validators.GetExitValidatorIndices())),
	}, nil
}

// GetEpochAssignments will get the epoch assignments from a Prysm client
func (pc *PrysmClient) GetEpochAssignments(epoch uint64) (*types.EpochAssignments, error) {

	pc.assignmentsCacheMux.Lock()
	defer pc.assignmentsCacheMux.Unlock()

	var err error

	cachedValue, found := pc.assignmentsCache.Get(epoch)
	if found {
		return cachedValue.(*types.EpochAssignments), nil
	}

	logger.Infof("caching assignments for epoch %v", epoch)
	start := time.Now()
	assignments := &types.EpochAssignments{
		ProposerAssignments: make(map[uint64]uint64),
		AttestorAssignments: make(map[string]uint64),
	}

	// Retrieve the validator assignments for the epoch
	validatorAssignmentes := make([]*ethpb.ValidatorAssignments_CommitteeAssignment, 0)
	validatorAssignmentResponse := &ethpb.ValidatorAssignments{}
	validatorAssignmentRequest := &ethpb.ListValidatorAssignmentsRequest{PageToken: validatorAssignmentResponse.NextPageToken, PageSize: utils.Config.Indexer.Node.PageSize, QueryFilter: &ethpb.ListValidatorAssignmentsRequest_Epoch{Epoch: eth2types.Epoch(epoch)}}
	if epoch == 0 {
		validatorAssignmentRequest.QueryFilter = &ethpb.ListValidatorAssignmentsRequest_Genesis{Genesis: true}
	}
	for {
		validatorAssignmentRequest.PageToken = validatorAssignmentResponse.NextPageToken
		validatorAssignmentResponse, err = pc.client.ListValidatorAssignments(context.Background(), validatorAssignmentRequest)
		if err != nil {
			return nil, fmt.Errorf("error retrieving validator assignment response for caching: %v", err)
		}

		validatorAssignmentes = append(validatorAssignmentes, validatorAssignmentResponse.Assignments...)
		//logger.Printf("retrieved %v assignments of %v for epoch %v", len(validatorAssignmentes), validatorAssignmentResponse.TotalSize, epoch)

		if validatorAssignmentResponse.NextPageToken == "" || validatorAssignmentResponse.TotalSize == 0 || len(validatorAssignmentes) == int(validatorAssignmentResponse.TotalSize) {
			break
		}
	}

	// Extract the proposer & attestation assignments from the response and cache them for later use
	// Proposer assignments are cached by the proposer slot
	// Attestation assignments are cached by the slot & committee key
	for _, assignment := range validatorAssignmentes {
		for _, slot := range assignment.ProposerSlots {
			assignments.ProposerAssignments[uint64(slot)] = uint64(assignment.ValidatorIndex)
		}

		for memberIndex, validatorIndex := range assignment.BeaconCommittees {
			assignments.AttestorAssignments[utils.FormatAttestorAssignmentKey(uint64(assignment.AttesterSlot), uint64(assignment.CommitteeIndex), uint64(memberIndex))] = uint64(validatorIndex)
		}
	}

	if len(assignments.AttestorAssignments) > 0 && len(assignments.ProposerAssignments) > 0 {
		pc.assignmentsCache.Add(epoch, assignments)
	}

	logger.Infof("cached assignments for epoch %v took %v", epoch, time.Since(start))
	return assignments, nil
}

// GetEpochData will get the epoch data from a Prysm client
func (pc *PrysmClient) GetEpochData(epoch uint64) (*types.EpochData, error) {
	wg := &sync.WaitGroup{}
	var err error

	data := &types.EpochData{}
	data.Epoch = epoch

	var lastSlot uint64
	if PrysmLatestHeadSlot == 0 {
		lastSlot = epoch * utils.Config.Chain.Config.SlotsPerEpoch
	} else {
		lastSlot = (epoch+1)*utils.Config.Chain.Config.SlotsPerEpoch - 1
		if PrysmLatestHeadSlot < lastSlot {
			lastSlot = PrysmLatestHeadSlot
		}
	}

	validatorsResp, err := pc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%d/validators", pc.endpoint, lastSlot))
	if err != nil {
		return nil, fmt.Errorf("error retrieving validators for slot %v: %v", lastSlot, err)
	}
	var parsedValidators StandardValidatorsResponse
	err = json.Unmarshal(validatorsResp, &parsedValidators)
	if err != nil {
		return nil, fmt.Errorf("error parsing epoch validators: %v", err)
	}

	slot1d := int64(lastSlot) - 7200
	slot7d := int64(lastSlot) - 7200*7
	slot31d := int64(lastSlot) - 7200*31

	if slot1d < 0 {
		slot1d = 0
	}
	if slot7d < 0 {
		slot7d = 0
	}
	if slot31d < 0 {
		slot31d = 0
	}

	var validatorBalances1d map[uint64]uint64
	var validatorBalances7d map[uint64]uint64
	var validatorBalances31d map[uint64]uint64
	var validatorWithdrawal map[uint64]uint64
	var validatorWithdrawal1d map[uint64]uint64
	var validatorWithdrawal7d map[uint64]uint64
	var validatorWithdrawal31d map[uint64]uint64

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorWithdrawal, err = db.GetAllValidatorTotalWithdrawals(uint64(lastSlot))
		if err != nil {
			logrus.Errorf("error retrieving validator total withdrawal for slot %v : %v", lastSlot, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for slot %v (1d) took %v", len(parsedValidators.Data), slot1d, time.Since(start))
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances1d, err = pc.GetBalancesForSlot(slot1d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for slot %v (1d): %v", slot1d, err)
			return
		}
		validatorWithdrawal1d, err = db.GetAllValidatorTotalWithdrawals(uint64(slot1d))
		if err != nil {
			logrus.Errorf("error retrieving validator total withdrawal for slot %v (1d): %v", slot1d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for slot %v (1d) took %v", len(parsedValidators.Data), slot1d, time.Since(start))
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances7d, err = pc.GetBalancesForSlot(slot7d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for slot %v (7d): %v", slot7d, err)
			return
		}
		validatorWithdrawal7d, err = db.GetAllValidatorTotalWithdrawals(uint64(slot7d))
		if err != nil {
			logrus.Errorf("error retrieving validator total withdrawal for slot %v (7d): %v", slot7d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for slot %v (7d) took %v", len(parsedValidators.Data), slot7d, time.Since(start))
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances31d, err = pc.GetBalancesForSlot(slot31d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for slot %v (31d): %v", slot31d, err)
			return
		}
		validatorWithdrawal31d, err = db.GetAllValidatorTotalWithdrawals(uint64(slot31d))
		if err != nil {
			logrus.Errorf("error retrieving validator total withdrawal for slot %v (31d): %v", slot31d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for slot %v (31d) took %v", len(parsedValidators.Data), slot31d, time.Since(start))
	}()
	wg.Wait()

	data.ValidatorAssignmentes, err = pc.GetEpochAssignments(epoch)
	if err != nil {
		return nil, fmt.Errorf("error retrieving assignments for epoch %v: %v", epoch, err)
	}
	logger.Printf("retrieved validator assignment data for epoch %v", epoch)

	// Retrieve all blocks for the epoch
	data.Blocks = make(map[uint64]map[string]*types.Block)

	for slot := epoch * utils.Config.Chain.Config.SlotsPerEpoch; slot <= (epoch+1)*utils.Config.Chain.Config.SlotsPerEpoch-1; slot++ {
		blocks, err := pc.GetBlocksBySlot(slot)

		if err != nil {
			return nil, err
		}

		for _, block := range blocks {
			if data.Blocks[block.Slot] == nil {
				data.Blocks[block.Slot] = make(map[string]*types.Block)
			}
			data.Blocks[block.Slot][fmt.Sprintf("%x", block.BlockRoot)] = block
		}
	}
	logger.Printf("retrieved %v blocks for epoch %v", len(data.Blocks), epoch)

	slots := make([]uint64, 0, len(data.Blocks))
	for slot := range data.Blocks {
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool {
		return slots[i] < slots[j]
	})

	for _, slot := range slots {
		if slot > PrysmLatestHeadSlot {
			for _, b := range data.Blocks[slot] {
				if payload := b.ExecutionPayload; payload != nil && payload.Withdrawals != nil {
					for _, wd := range payload.Withdrawals {
						value, exists := validatorWithdrawal[wd.ValidatorIndex]
						if exists {
							validatorWithdrawal[wd.ValidatorIndex] = value + wd.Amount
						} else {
							validatorWithdrawal[wd.ValidatorIndex] = wd.Amount
						}
					}
				}
			}
		}
	}

	// Fill up missed and scheduled blocks
	for slot, proposer := range data.ValidatorAssignmentes.ProposerAssignments {
		_, found := data.Blocks[slot]
		if !found {
			// Proposer was assigned but did not yet propose a block
			data.Blocks[slot] = make(map[string]*types.Block)
			data.Blocks[slot]["0x0"] = &types.Block{
				Status:            0,
				Proposer:          proposer,
				BlockRoot:         []byte{0x0},
				Slot:              slot,
				ParentRoot:        []byte{},
				StateRoot:         []byte{},
				Signature:         []byte{},
				RandaoReveal:      []byte{},
				Graffiti:          []byte{},
				BodyRoot:          []byte{},
				Eth1Data:          &types.Eth1Data{},
				ProposerSlashings: make([]*types.ProposerSlashing, 0),
				AttesterSlashings: make([]*types.AttesterSlashing, 0),
				Attestations:      make([]*types.Attestation, 0),
				Deposits:          make([]*types.Deposit, 0),
				VoluntaryExits:    make([]*types.VoluntaryExit, 0),
			}

			if utils.SlotToTime(slot).After(time.Now().Add(time.Second * -60)) {
				// Block is in the future, set status to scheduled
				data.Blocks[slot]["0x0"].Status = 0
				data.Blocks[slot]["0x0"].BlockRoot = []byte{0x0}
			} else {
				// Block is in the past, set status to missed
				data.Blocks[slot]["0x0"].Status = 2
				data.Blocks[slot]["0x0"].BlockRoot = []byte{0x1}
			}
		}
	}

	// Retrieve the validator set for the epoch
	data.Validators = make([]*types.Validator, 0)

	for _, validator := range parsedValidators.Data {
		data.Validators = append(data.Validators, &types.Validator{
			Index:                      uint64(validator.Index),
			PublicKey:                  utils.MustParseHex(validator.Validator.Pubkey),
			WithdrawalCredentials:      utils.MustParseHex(validator.Validator.WithdrawalCredentials),
			Balance:                    uint64(validator.Balance),
			EffectiveBalance:           uint64(validator.Validator.EffectiveBalance),
			Slashed:                    validator.Validator.Slashed,
			ActivationEligibilityEpoch: uint64(validator.Validator.ActivationEligibilityEpoch),
			ActivationEpoch:            uint64(validator.Validator.ActivationEpoch),
			ExitEpoch:                  uint64(validator.Validator.ExitEpoch),
			WithdrawableEpoch:          uint64(validator.Validator.WithdrawableEpoch),
			Balance1d:                  validatorBalances1d[uint64(validator.Index)],
			Balance7d:                  validatorBalances7d[uint64(validator.Index)],
			Balance31d:                 validatorBalances31d[uint64(validator.Index)],
			Withdrawal:                 validatorWithdrawal[uint64(validator.Index)],
			Withdrawal1d:               validatorWithdrawal1d[uint64(validator.Index)],
			Withdrawal7d:               validatorWithdrawal7d[uint64(validator.Index)],
			Withdrawal31d:              validatorWithdrawal31d[uint64(validator.Index)],
			Status:                     validator.Status,
		})
	}
	logger.Printf("retrieved data for %v validators for epoch %v", len(data.Validators), epoch)

	data.EpochParticipationStats, err = pc.GetValidatorParticipation(epoch)
	if err != nil {
		return nil, fmt.Errorf("error retrieving epoch participation statistics for epoch %v: %v", epoch, err)
	}

	return data, nil
}

func (pc *PrysmClient) GetBalancesForEpoch(epoch int64) (map[uint64]uint64, error) {

	if epoch < 0 {
		epoch = 0
	}

	var err error

	validatorBalances := make(map[uint64]uint64)

	validatorBalancesResponse := &ethpb.ValidatorBalances{}
	validatorBalancesRequest := &ethpb.ListValidatorBalancesRequest{
		PageSize:    utils.Config.Indexer.Node.PageSize,
		PageToken:   validatorBalancesResponse.NextPageToken,
		QueryFilter: &ethpb.ListValidatorBalancesRequest_Epoch{Epoch: eth2types.Epoch(epoch)}}
	if epoch == 0 {
		validatorBalancesRequest.QueryFilter = &ethpb.ListValidatorBalancesRequest_Genesis{Genesis: true}
	}
	for {
		validatorBalancesRequest.PageToken = validatorBalancesResponse.NextPageToken
		validatorBalancesResponse, err = pc.client.ListValidatorBalances(context.Background(), validatorBalancesRequest)
		if err != nil {
			logger.Printf("error retrieving validator balances for epoch %v: %v", epoch, err)
			break
		}
		if validatorBalancesResponse.TotalSize == 0 {
			break
		}

		for _, balance := range validatorBalancesResponse.Balances {
			validatorBalances[uint64(balance.Index)] = balance.Balance
		}

		if validatorBalancesResponse.NextPageToken == "" {
			break
		}
	}
	return validatorBalances, err
}

// GetBlocksBySlot will get blocks by slot from a Prysm client
func (pc *PrysmClient) GetBlocksBySlot(slot uint64) ([]*types.Block, error) {
	logger.Infof("retrieving block at slot %v", slot)

	blocks := make([]*types.Block, 0)

	blocksRequest := &ethpb.ListBlocksRequest{PageSize: utils.Config.Indexer.Node.PageSize, QueryFilter: &ethpb.ListBlocksRequest_Slot{Slot: eth2types.Slot(slot)}}
	if slot == 0 {
		blocksRequest.QueryFilter = &ethpb.ListBlocksRequest_Genesis{Genesis: true}
	}

	// blocksResponse, err := pc.client.ListBlocks(context.Background(), blocksRequest)
	blocksResponse, err := pc.client.ListBeaconBlocks(context.Background(), blocksRequest)
	if err != nil {
		return nil, err
	}

	if blocksResponse.TotalSize == 0 {
		return blocks, nil
	}

	for _, block := range blocksResponse.BlockContainers {
		// Make sure that blocks from the genesis epoch have their Eth1Data field set
		blk := block.GetAltairBlock()
		if blk != nil && blk.Block.Body.Eth1Data == nil {
			blk.Block.Body.Eth1Data = &ethpb.Eth1Data{
				DepositRoot:  []byte{},
				DepositCount: 0,
				BlockHash:    []byte{},
			}
		}

		b, err := pc.parseRpcBlock(block)
		if err != nil {
			return nil, err
		}

		blocks = append(blocks, b)
	}

	return blocks, nil
}

// GetBlockStatusBySlot will get blocks by slot from a Prysm client
func (pc *PrysmClient) GetBlockStatusByEpoch(epoch uint64) ([]*types.CanonBlock, error) {
	logger.Infof("retrieving blocks for epoch %v", epoch)

	blocks := make([]*types.CanonBlock, 0)

	blocksRequest := &ethpb.ListBlocksRequest{PageSize: utils.Config.Indexer.Node.PageSize, QueryFilter: &ethpb.ListBlocksRequest_Epoch{Epoch: eth2types.Epoch(epoch)}}

	blocksResponse, err := pc.client.ListBeaconBlocks(context.Background(), blocksRequest)
	if err != nil {
		return nil, err
	}

	if blocksResponse.TotalSize == 0 {
		return blocks, nil
	}

	for _, block := range blocksResponse.BlockContainers {
		var slot eth2types.Slot = 0

		if altairBlock := block.GetAltairBlock(); altairBlock != nil {
			slot = altairBlock.Block.GetSlot()
		} else {
			slot = block.GetPhase0Block().GetBlock().GetSlot()
		}

		blocks = append(blocks, &types.CanonBlock{
			BlockRoot: block.BlockRoot,
			Slot:      uint64(slot),
			Canonical: block.Canonical,
		})
	}

	return blocks, nil
}

func (pc *PrysmClient) parseRpcBlock(block *ethpb.BeaconBlockContainer) (*types.Block, error) {
	phase0Block := block.GetPhase0Block()
	if phase0Block != nil {
		return pc.parsePhase0Block(block)
	}
	altairBlock := block.GetAltairBlock()
	if altairBlock != nil {
		return pc.parseAltairBlock(block)
	}
	bellatrixBlock := block.GetBellatrixBlock()
	if bellatrixBlock != nil {
		return pc.parseBellatrixBlock(block)
	}
	capellaBlock := block.GetCapellaBlock()
	if capellaBlock != nil {
		return pc.parseCapellaBlock(block)
	}
	return nil, fmt.Errorf("block is neither phase0 nor altair nor bellatrix nor capella")
}

func (pc *PrysmClient) parsePhase0Block(block *ethpb.BeaconBlockContainer) (*types.Block, error) {
	blk := block.GetPhase0Block()
	if blk == nil {
		return nil, fmt.Errorf("failed getting phase0 block")
	}
	b := &types.Block{
		Status:       1,
		Canonical:    block.Canonical,
		BlockRoot:    block.BlockRoot,
		Slot:         uint64(blk.Block.Slot),
		ParentRoot:   blk.Block.ParentRoot,
		StateRoot:    blk.Block.StateRoot,
		Signature:    blk.Signature,
		RandaoReveal: blk.Block.Body.RandaoReveal,
		Graffiti:     blk.Block.Body.Graffiti,
		Eth1Data: &types.Eth1Data{
			DepositRoot:  blk.Block.Body.Eth1Data.DepositRoot,
			DepositCount: blk.Block.Body.Eth1Data.DepositCount,
			BlockHash:    blk.Block.Body.Eth1Data.BlockHash,
		},
		ProposerSlashings: make([]*types.ProposerSlashing, len(blk.Block.Body.ProposerSlashings)),
		AttesterSlashings: make([]*types.AttesterSlashing, len(blk.Block.Body.AttesterSlashings)),
		Attestations:      make([]*types.Attestation, len(blk.Block.Body.Attestations)),
		Deposits:          make([]*types.Deposit, len(blk.Block.Body.Deposits)),
		VoluntaryExits:    make([]*types.VoluntaryExit, len(blk.Block.Body.VoluntaryExits)),
		Proposer:          uint64(blk.Block.ProposerIndex),
	}

	for i, proposerSlashing := range blk.Block.Body.ProposerSlashings {
		b.ProposerSlashings[i] = &types.ProposerSlashing{
			ProposerIndex: uint64(proposerSlashing.Header_1.Header.ProposerIndex),
			Header1: &types.Block{
				Slot:       uint64(proposerSlashing.Header_1.Header.Slot),
				ParentRoot: proposerSlashing.Header_1.Header.ParentRoot,
				StateRoot:  proposerSlashing.Header_1.Header.StateRoot,
				Signature:  proposerSlashing.Header_1.Signature,
				BodyRoot:   proposerSlashing.Header_1.Header.BodyRoot,
			},
			Header2: &types.Block{
				Slot:       uint64(proposerSlashing.Header_2.Header.Slot),
				ParentRoot: proposerSlashing.Header_2.Header.ParentRoot,
				StateRoot:  proposerSlashing.Header_2.Header.StateRoot,
				Signature:  proposerSlashing.Header_2.Signature,
				BodyRoot:   proposerSlashing.Header_2.Header.BodyRoot,
			},
		}
	}

	for i, attesterSlashing := range blk.Block.Body.AttesterSlashings {
		b.AttesterSlashings[i] = &types.AttesterSlashing{
			Attestation1: &types.IndexedAttestation{
				Data: &types.AttestationData{
					Slot:            uint64(attesterSlashing.Attestation_1.Data.Slot),
					CommitteeIndex:  uint64(attesterSlashing.Attestation_1.Data.CommitteeIndex),
					BeaconBlockRoot: attesterSlashing.Attestation_1.Data.BeaconBlockRoot,
					Source: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_1.Data.Source.Epoch),
						Root:  attesterSlashing.Attestation_1.Data.Source.Root,
					},
					Target: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_1.Data.Target.Epoch),
						Root:  attesterSlashing.Attestation_1.Data.Target.Root,
					},
				},
				Signature:        attesterSlashing.Attestation_1.Signature,
				AttestingIndices: attesterSlashing.Attestation_1.AttestingIndices,
			},
			Attestation2: &types.IndexedAttestation{
				Data: &types.AttestationData{
					Slot:            uint64(attesterSlashing.Attestation_2.Data.Slot),
					CommitteeIndex:  uint64(attesterSlashing.Attestation_2.Data.CommitteeIndex),
					BeaconBlockRoot: attesterSlashing.Attestation_2.Data.BeaconBlockRoot,
					Source: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_2.Data.Source.Epoch),
						Root:  attesterSlashing.Attestation_2.Data.Source.Root,
					},
					Target: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_2.Data.Target.Epoch),
						Root:  attesterSlashing.Attestation_2.Data.Target.Root,
					},
				},
				Signature:        attesterSlashing.Attestation_2.Signature,
				AttestingIndices: attesterSlashing.Attestation_2.AttestingIndices,
			},
		}
	}

	for i, attestation := range blk.Block.Body.Attestations {
		a := &types.Attestation{
			AggregationBits: attestation.AggregationBits,
			Data: &types.AttestationData{
				Slot:            uint64(attestation.Data.Slot),
				CommitteeIndex:  uint64(attestation.Data.CommitteeIndex),
				BeaconBlockRoot: attestation.Data.BeaconBlockRoot,
				Source: &types.Checkpoint{
					Epoch: uint64(attestation.Data.Source.Epoch),
					Root:  attestation.Data.Source.Root,
				},
				Target: &types.Checkpoint{
					Epoch: uint64(attestation.Data.Target.Epoch),
					Root:  attestation.Data.Target.Root,
				},
			},
			Signature: attestation.Signature,
		}

		aggregationBits := bitfield.Bitlist(a.AggregationBits)
		assignments, err := pc.GetEpochAssignments(a.Data.Slot / utils.Config.Chain.Config.SlotsPerEpoch)
		if err != nil {
			return nil, fmt.Errorf("error receiving epoch assignment for epoch %v: %v", a.Data.Slot/utils.Config.Chain.Config.SlotsPerEpoch, err)
		}

		a.Attesters = make([]uint64, 0)
		for i := uint64(0); i < aggregationBits.Len(); i++ {
			if aggregationBits.BitAt(i) {
				validator, found := assignments.AttestorAssignments[utils.FormatAttestorAssignmentKey(a.Data.Slot, a.Data.CommitteeIndex, i)]
				if !found { // This should never happen!
					validator = 0
					logger.Errorf("error retrieving assigned validator for attestation %v of block %v for slot %v committee index %v member index %v", i, b.Slot, a.Data.Slot, a.Data.CommitteeIndex, i)
				}
				a.Attesters = append(a.Attesters, validator)
			}
		}

		b.Attestations[i] = a
	}
	for i, deposit := range blk.Block.Body.Deposits {
		b.Deposits[i] = &types.Deposit{
			Proof:                 deposit.Proof,
			PublicKey:             deposit.Data.PublicKey,
			WithdrawalCredentials: deposit.Data.WithdrawalCredentials,
			Amount:                deposit.Data.Amount,
			Signature:             deposit.Data.Signature,
		}
	}

	for i, voluntaryExit := range blk.Block.Body.VoluntaryExits {
		b.VoluntaryExits[i] = &types.VoluntaryExit{
			Epoch:          uint64(voluntaryExit.Exit.Epoch),
			ValidatorIndex: uint64(voluntaryExit.Exit.ValidatorIndex),
			Signature:      voluntaryExit.Signature,
		}
	}
	return b, nil
}

func (pc *PrysmClient) parseAltairBlock(block *ethpb.BeaconBlockContainer) (*types.Block, error) {
	blk := block.GetAltairBlock()
	if blk == nil {
		return nil, fmt.Errorf("failed getting altair block")
	}
	b := &types.Block{
		Status:       1,
		Canonical:    block.Canonical,
		BlockRoot:    block.BlockRoot,
		Slot:         uint64(blk.Block.Slot),
		ParentRoot:   blk.Block.ParentRoot,
		StateRoot:    blk.Block.StateRoot,
		Signature:    blk.Signature,
		RandaoReveal: blk.Block.Body.RandaoReveal,
		Graffiti:     blk.Block.Body.Graffiti,
		Eth1Data: &types.Eth1Data{
			DepositRoot:  blk.Block.Body.Eth1Data.DepositRoot,
			DepositCount: blk.Block.Body.Eth1Data.DepositCount,
			BlockHash:    blk.Block.Body.Eth1Data.BlockHash,
		},
		ProposerSlashings: make([]*types.ProposerSlashing, len(blk.Block.Body.ProposerSlashings)),
		AttesterSlashings: make([]*types.AttesterSlashing, len(blk.Block.Body.AttesterSlashings)),
		Attestations:      make([]*types.Attestation, len(blk.Block.Body.Attestations)),
		Deposits:          make([]*types.Deposit, len(blk.Block.Body.Deposits)),
		VoluntaryExits:    make([]*types.VoluntaryExit, len(blk.Block.Body.VoluntaryExits)),
		Proposer:          uint64(blk.Block.ProposerIndex),
	}

	if blk.Block.Body.SyncAggregate != nil {
		bits := blk.Block.Body.SyncAggregate.SyncCommitteeBits.Bytes()
		b.SyncAggregate = &types.SyncAggregate{
			SyncCommitteeBits:          bits,
			SyncAggregateParticipation: syncCommitteeParticipation(bits),
			SyncCommitteeSignature:     blk.Block.Body.SyncAggregate.SyncCommitteeSignature,
		}
	}

	for i, proposerSlashing := range blk.Block.Body.ProposerSlashings {
		b.ProposerSlashings[i] = &types.ProposerSlashing{
			ProposerIndex: uint64(proposerSlashing.Header_1.Header.ProposerIndex),
			Header1: &types.Block{
				Slot:       uint64(proposerSlashing.Header_1.Header.Slot),
				ParentRoot: proposerSlashing.Header_1.Header.ParentRoot,
				StateRoot:  proposerSlashing.Header_1.Header.StateRoot,
				Signature:  proposerSlashing.Header_1.Signature,
				BodyRoot:   proposerSlashing.Header_1.Header.BodyRoot,
			},
			Header2: &types.Block{
				Slot:       uint64(proposerSlashing.Header_2.Header.Slot),
				ParentRoot: proposerSlashing.Header_2.Header.ParentRoot,
				StateRoot:  proposerSlashing.Header_2.Header.StateRoot,
				Signature:  proposerSlashing.Header_2.Signature,
				BodyRoot:   proposerSlashing.Header_2.Header.BodyRoot,
			},
		}
	}

	for i, attesterSlashing := range blk.Block.Body.AttesterSlashings {
		b.AttesterSlashings[i] = &types.AttesterSlashing{
			Attestation1: &types.IndexedAttestation{
				Data: &types.AttestationData{
					Slot:            uint64(attesterSlashing.Attestation_1.Data.Slot),
					CommitteeIndex:  uint64(attesterSlashing.Attestation_1.Data.CommitteeIndex),
					BeaconBlockRoot: attesterSlashing.Attestation_1.Data.BeaconBlockRoot,
					Source: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_1.Data.Source.Epoch),
						Root:  attesterSlashing.Attestation_1.Data.Source.Root,
					},
					Target: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_1.Data.Target.Epoch),
						Root:  attesterSlashing.Attestation_1.Data.Target.Root,
					},
				},
				Signature:        attesterSlashing.Attestation_1.Signature,
				AttestingIndices: attesterSlashing.Attestation_1.AttestingIndices,
			},
			Attestation2: &types.IndexedAttestation{
				Data: &types.AttestationData{
					Slot:            uint64(attesterSlashing.Attestation_2.Data.Slot),
					CommitteeIndex:  uint64(attesterSlashing.Attestation_2.Data.CommitteeIndex),
					BeaconBlockRoot: attesterSlashing.Attestation_2.Data.BeaconBlockRoot,
					Source: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_2.Data.Source.Epoch),
						Root:  attesterSlashing.Attestation_2.Data.Source.Root,
					},
					Target: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_2.Data.Target.Epoch),
						Root:  attesterSlashing.Attestation_2.Data.Target.Root,
					},
				},
				Signature:        attesterSlashing.Attestation_2.Signature,
				AttestingIndices: attesterSlashing.Attestation_2.AttestingIndices,
			},
		}
	}

	for i, attestation := range blk.Block.Body.Attestations {
		a := &types.Attestation{
			AggregationBits: attestation.AggregationBits,
			Data: &types.AttestationData{
				Slot:            uint64(attestation.Data.Slot),
				CommitteeIndex:  uint64(attestation.Data.CommitteeIndex),
				BeaconBlockRoot: attestation.Data.BeaconBlockRoot,
				Source: &types.Checkpoint{
					Epoch: uint64(attestation.Data.Source.Epoch),
					Root:  attestation.Data.Source.Root,
				},
				Target: &types.Checkpoint{
					Epoch: uint64(attestation.Data.Target.Epoch),
					Root:  attestation.Data.Target.Root,
				},
			},
			Signature: attestation.Signature,
		}

		aggregationBits := bitfield.Bitlist(a.AggregationBits)
		assignments, err := pc.GetEpochAssignments(a.Data.Slot / utils.Config.Chain.Config.SlotsPerEpoch)
		if err != nil {
			return nil, fmt.Errorf("error receiving epoch assignment for epoch %v: %v", a.Data.Slot/utils.Config.Chain.Config.SlotsPerEpoch, err)
		}

		a.Attesters = make([]uint64, 0)
		for i := uint64(0); i < aggregationBits.Len(); i++ {
			if aggregationBits.BitAt(i) {
				validator, found := assignments.AttestorAssignments[utils.FormatAttestorAssignmentKey(a.Data.Slot, a.Data.CommitteeIndex, i)]
				if !found { // This should never happen!
					validator = 0
					logger.Errorf("error retrieving assigned validator for attestation %v of block %v for slot %v committee index %v member index %v", i, b.Slot, a.Data.Slot, a.Data.CommitteeIndex, i)
				}
				a.Attesters = append(a.Attesters, validator)
			}
		}

		b.Attestations[i] = a
	}
	for i, deposit := range blk.Block.Body.Deposits {
		b.Deposits[i] = &types.Deposit{
			Proof:                 deposit.Proof,
			PublicKey:             deposit.Data.PublicKey,
			WithdrawalCredentials: deposit.Data.WithdrawalCredentials,
			Amount:                deposit.Data.Amount,
			Signature:             deposit.Data.Signature,
		}
	}

	for i, voluntaryExit := range blk.Block.Body.VoluntaryExits {
		b.VoluntaryExits[i] = &types.VoluntaryExit{
			Epoch:          uint64(voluntaryExit.Exit.Epoch),
			ValidatorIndex: uint64(voluntaryExit.Exit.ValidatorIndex),
			Signature:      voluntaryExit.Signature,
		}
	}
	return b, nil
}

func (pc *PrysmClient) parseBellatrixBlock(block *ethpb.BeaconBlockContainer) (*types.Block, error) {
	blk := block.GetBellatrixBlock()
	if blk == nil {
		return nil, fmt.Errorf("failed getting bellatrix block")
	}
	b := &types.Block{
		Status:       1,
		Canonical:    block.Canonical,
		BlockRoot:    block.BlockRoot,
		Slot:         uint64(blk.Block.Slot),
		ParentRoot:   blk.Block.ParentRoot,
		StateRoot:    blk.Block.StateRoot,
		Signature:    blk.Signature,
		RandaoReveal: blk.Block.Body.RandaoReveal,
		Graffiti:     blk.Block.Body.Graffiti,
		Eth1Data: &types.Eth1Data{
			DepositRoot:  blk.Block.Body.Eth1Data.DepositRoot,
			DepositCount: blk.Block.Body.Eth1Data.DepositCount,
			BlockHash:    blk.Block.Body.Eth1Data.BlockHash,
		},
		ProposerSlashings: make([]*types.ProposerSlashing, len(blk.Block.Body.ProposerSlashings)),
		AttesterSlashings: make([]*types.AttesterSlashing, len(blk.Block.Body.AttesterSlashings)),
		Attestations:      make([]*types.Attestation, len(blk.Block.Body.Attestations)),
		Deposits:          make([]*types.Deposit, len(blk.Block.Body.Deposits)),
		VoluntaryExits:    make([]*types.VoluntaryExit, len(blk.Block.Body.VoluntaryExits)),
		Proposer:          uint64(blk.Block.ProposerIndex),
	}

	if payload := blk.Block.Body.ExecutionPayload; payload != nil && !bytes.Equal(payload.ParentHash, make([]byte, 32)) {
		txs := make([]*types.Transaction, 0, len(payload.Transactions))
		for i, rawTx := range payload.Transactions {
			tx := &types.Transaction{Raw: rawTx}
			var decTx gtypes.Transaction
			if err := decTx.UnmarshalBinary(rawTx); err != nil {
				return nil, fmt.Errorf("error parsing tx %d block %x: %v", i, payload.BlockHash, err)
			} else {
				h := decTx.Hash()
				tx.TxHash = h[:]
				tx.AccountNonce = decTx.Nonce()
				// big endian
				tx.Price = decTx.GasPrice().Bytes()
				tx.GasLimit = decTx.Gas()
				sender, err := pc.signer.Sender(&decTx)
				if err != nil {
					return nil, fmt.Errorf("transaction with invalid sender (tx hash: %x): %v", h, err)
				}
				tx.Sender = sender.Bytes()
				if v := decTx.To(); v != nil {
					tx.Recipient = v.Bytes()
				} else {
					tx.Recipient = []byte{}
				}
				tx.Amount = decTx.Value().Bytes()
				tx.Payload = decTx.Data()
				tx.MaxPriorityFeePerGas = decTx.GasTipCap().Uint64()
				tx.MaxFeePerGas = decTx.GasFeeCap().Uint64()
			}
			txs = append(txs, tx)
		}

		b.ExecutionPayload = &types.ExecutionPayload{
			ParentHash:    payload.ParentHash,
			FeeRecipient:  payload.FeeRecipient,
			StateRoot:     payload.StateRoot,
			ReceiptsRoot:  payload.ReceiptsRoot,
			LogsBloom:     payload.LogsBloom,
			Random:        payload.PrevRandao,
			BlockNumber:   payload.BlockNumber,
			GasLimit:      payload.GasLimit,
			GasUsed:       payload.GasUsed,
			Timestamp:     payload.Timestamp,
			ExtraData:     payload.ExtraData,
			BaseFeePerGas: binary.LittleEndian.Uint64(payload.BaseFeePerGas), // TODO, this is problematic
			BlockHash:     payload.BlockHash,
			Transactions:  txs,
			Withdrawals:   nil,
		}
	}

	if blk.Block.Body.SyncAggregate != nil {
		bits := blk.Block.Body.SyncAggregate.SyncCommitteeBits.Bytes()
		b.SyncAggregate = &types.SyncAggregate{
			SyncCommitteeBits:          bits,
			SyncAggregateParticipation: syncCommitteeParticipation(bits),
			SyncCommitteeSignature:     blk.Block.Body.SyncAggregate.SyncCommitteeSignature,
		}
	}

	for i, proposerSlashing := range blk.Block.Body.ProposerSlashings {
		b.ProposerSlashings[i] = &types.ProposerSlashing{
			ProposerIndex: uint64(proposerSlashing.Header_1.Header.ProposerIndex),
			Header1: &types.Block{
				Slot:       uint64(proposerSlashing.Header_1.Header.Slot),
				ParentRoot: proposerSlashing.Header_1.Header.ParentRoot,
				StateRoot:  proposerSlashing.Header_1.Header.StateRoot,
				Signature:  proposerSlashing.Header_1.Signature,
				BodyRoot:   proposerSlashing.Header_1.Header.BodyRoot,
			},
			Header2: &types.Block{
				Slot:       uint64(proposerSlashing.Header_2.Header.Slot),
				ParentRoot: proposerSlashing.Header_2.Header.ParentRoot,
				StateRoot:  proposerSlashing.Header_2.Header.StateRoot,
				Signature:  proposerSlashing.Header_2.Signature,
				BodyRoot:   proposerSlashing.Header_2.Header.BodyRoot,
			},
		}
	}

	for i, attesterSlashing := range blk.Block.Body.AttesterSlashings {
		b.AttesterSlashings[i] = &types.AttesterSlashing{
			Attestation1: &types.IndexedAttestation{
				Data: &types.AttestationData{
					Slot:            uint64(attesterSlashing.Attestation_1.Data.Slot),
					CommitteeIndex:  uint64(attesterSlashing.Attestation_1.Data.CommitteeIndex),
					BeaconBlockRoot: attesterSlashing.Attestation_1.Data.BeaconBlockRoot,
					Source: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_1.Data.Source.Epoch),
						Root:  attesterSlashing.Attestation_1.Data.Source.Root,
					},
					Target: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_1.Data.Target.Epoch),
						Root:  attesterSlashing.Attestation_1.Data.Target.Root,
					},
				},
				Signature:        attesterSlashing.Attestation_1.Signature,
				AttestingIndices: attesterSlashing.Attestation_1.AttestingIndices,
			},
			Attestation2: &types.IndexedAttestation{
				Data: &types.AttestationData{
					Slot:            uint64(attesterSlashing.Attestation_2.Data.Slot),
					CommitteeIndex:  uint64(attesterSlashing.Attestation_2.Data.CommitteeIndex),
					BeaconBlockRoot: attesterSlashing.Attestation_2.Data.BeaconBlockRoot,
					Source: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_2.Data.Source.Epoch),
						Root:  attesterSlashing.Attestation_2.Data.Source.Root,
					},
					Target: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_2.Data.Target.Epoch),
						Root:  attesterSlashing.Attestation_2.Data.Target.Root,
					},
				},
				Signature:        attesterSlashing.Attestation_2.Signature,
				AttestingIndices: attesterSlashing.Attestation_2.AttestingIndices,
			},
		}
	}

	for i, attestation := range blk.Block.Body.Attestations {
		a := &types.Attestation{
			AggregationBits: attestation.AggregationBits,
			Data: &types.AttestationData{
				Slot:            uint64(attestation.Data.Slot),
				CommitteeIndex:  uint64(attestation.Data.CommitteeIndex),
				BeaconBlockRoot: attestation.Data.BeaconBlockRoot,
				Source: &types.Checkpoint{
					Epoch: uint64(attestation.Data.Source.Epoch),
					Root:  attestation.Data.Source.Root,
				},
				Target: &types.Checkpoint{
					Epoch: uint64(attestation.Data.Target.Epoch),
					Root:  attestation.Data.Target.Root,
				},
			},
			Signature: attestation.Signature,
		}

		aggregationBits := bitfield.Bitlist(a.AggregationBits)
		assignments, err := pc.GetEpochAssignments(a.Data.Slot / utils.Config.Chain.Config.SlotsPerEpoch)
		if err != nil {
			return nil, fmt.Errorf("error receiving epoch assignment for epoch %v: %v", a.Data.Slot/utils.Config.Chain.Config.SlotsPerEpoch, err)
		}

		a.Attesters = make([]uint64, 0)
		for i := uint64(0); i < aggregationBits.Len(); i++ {
			if aggregationBits.BitAt(i) {
				validator, found := assignments.AttestorAssignments[utils.FormatAttestorAssignmentKey(a.Data.Slot, a.Data.CommitteeIndex, i)]
				if !found { // This should never happen!
					validator = 0
					logger.Errorf("error retrieving assigned validator for attestation %v of block %v for slot %v committee index %v member index %v", i, b.Slot, a.Data.Slot, a.Data.CommitteeIndex, i)
				}
				a.Attesters = append(a.Attesters, validator)
			}
		}

		b.Attestations[i] = a
	}
	for i, deposit := range blk.Block.Body.Deposits {
		b.Deposits[i] = &types.Deposit{
			Proof:                 deposit.Proof,
			PublicKey:             deposit.Data.PublicKey,
			WithdrawalCredentials: deposit.Data.WithdrawalCredentials,
			Amount:                deposit.Data.Amount,
			Signature:             deposit.Data.Signature,
		}
	}

	for i, voluntaryExit := range blk.Block.Body.VoluntaryExits {
		b.VoluntaryExits[i] = &types.VoluntaryExit{
			Epoch:          uint64(voluntaryExit.Exit.Epoch),
			ValidatorIndex: uint64(voluntaryExit.Exit.ValidatorIndex),
			Signature:      voluntaryExit.Signature,
		}
	}
	return b, nil
}

func (pc *PrysmClient) parseCapellaBlock(block *ethpb.BeaconBlockContainer) (*types.Block, error) {
	blk := block.GetCapellaBlock()
	if blk == nil {
		return nil, fmt.Errorf("failed getting capella block")
	}
	b := &types.Block{
		Status:       1,
		Proposer:     uint64(blk.Block.ProposerIndex),
		BlockRoot:    block.BlockRoot,
		Slot:         uint64(blk.Block.Slot),
		ParentRoot:   blk.Block.ParentRoot,
		StateRoot:    blk.Block.StateRoot,
		Signature:    blk.Signature,
		RandaoReveal: blk.Block.Body.RandaoReveal,
		Graffiti:     blk.Block.Body.Graffiti,
		Eth1Data: &types.Eth1Data{
			DepositRoot:  blk.Block.Body.Eth1Data.DepositRoot,
			DepositCount: blk.Block.Body.Eth1Data.DepositCount,
			BlockHash:    blk.Block.Body.Eth1Data.BlockHash,
		},
		BodyRoot:                   nil,
		ProposerSlashings:          make([]*types.ProposerSlashing, len(blk.Block.Body.ProposerSlashings)),
		AttesterSlashings:          make([]*types.AttesterSlashing, len(blk.Block.Body.AttesterSlashings)),
		Attestations:               make([]*types.Attestation, len(blk.Block.Body.Attestations)),
		Deposits:                   make([]*types.Deposit, len(blk.Block.Body.Deposits)),
		VoluntaryExits:             make([]*types.VoluntaryExit, len(blk.Block.Body.VoluntaryExits)),
		SyncAggregate:              nil,
		ExecutionPayload:           nil,
		Canonical:                  block.Canonical,
		SignedBLSToExecutionChange: make([]*types.SignedBLSToExecutionChange, len(blk.Block.Body.BlsToExecutionChanges)),
	}

	// ExecutionPayload
	if payload := blk.Block.Body.ExecutionPayload; payload != nil && !bytes.Equal(payload.ParentHash, make([]byte, 32)) {
		txs := make([]*types.Transaction, 0, len(payload.Transactions))
		for i, rawTx := range payload.Transactions {
			tx := &types.Transaction{Raw: rawTx}
			var decTx gtypes.Transaction
			if err := decTx.UnmarshalBinary(rawTx); err != nil {
				return nil, fmt.Errorf("error parsing tx %d block %x: %v", i, payload.BlockHash, err)
			} else {
				h := decTx.Hash()
				tx.TxHash = h[:]
				tx.AccountNonce = decTx.Nonce()
				// big endian
				tx.Price = decTx.GasPrice().Bytes()
				tx.GasLimit = decTx.Gas()
				sender, err := pc.signer.Sender(&decTx)
				if err != nil {
					return nil, fmt.Errorf("transaction with invalid sender (tx hash: %x): %v", h, err)
				}
				tx.Sender = sender.Bytes()
				if v := decTx.To(); v != nil {
					tx.Recipient = v.Bytes()
				} else {
					tx.Recipient = []byte{}
				}
				tx.Amount = decTx.Value().Bytes()
				tx.Payload = decTx.Data()
				tx.MaxPriorityFeePerGas = decTx.GasTipCap().Uint64()
				tx.MaxFeePerGas = decTx.GasFeeCap().Uint64()
			}
			txs = append(txs, tx)
		}

		withdrawals := make([]*types.Withdrawals, 0, len(payload.Withdrawals))
		for _, rawWithdrawal := range payload.Withdrawals {
			withdraw := &types.Withdrawals{}
			withdraw.Slot = uint64(blk.Block.Slot)
			withdraw.BlockRoot = block.BlockRoot
			withdraw.Index = rawWithdrawal.Index
			withdraw.ValidatorIndex = uint64(rawWithdrawal.ValidatorIndex)
			withdraw.Address = rawWithdrawal.Address
			withdraw.Amount = rawWithdrawal.Amount
			withdrawals = append(withdrawals, withdraw)
		}

		b.ExecutionPayload = &types.ExecutionPayload{
			ParentHash:    payload.ParentHash,
			FeeRecipient:  payload.FeeRecipient,
			StateRoot:     payload.StateRoot,
			ReceiptsRoot:  payload.ReceiptsRoot,
			LogsBloom:     payload.LogsBloom,
			Random:        payload.PrevRandao,
			BlockNumber:   payload.BlockNumber,
			GasLimit:      payload.GasLimit,
			GasUsed:       payload.GasUsed,
			Timestamp:     payload.Timestamp,
			ExtraData:     payload.ExtraData,
			BaseFeePerGas: binary.LittleEndian.Uint64(payload.BaseFeePerGas), // TODO, this is problematic
			BlockHash:     payload.BlockHash,
			Transactions:  txs,
			Withdrawals:   withdrawals,
		}
	}

	// SyncAggregate
	if blk.Block.Body.SyncAggregate != nil {
		bits := blk.Block.Body.SyncAggregate.SyncCommitteeBits.Bytes()
		b.SyncAggregate = &types.SyncAggregate{
			SyncCommitteeBits:          bits,
			SyncAggregateParticipation: syncCommitteeParticipation(bits),
			SyncCommitteeSignature:     blk.Block.Body.SyncAggregate.SyncCommitteeSignature,
		}
	}

	// ProposerSlashings
	for i, proposerSlashing := range blk.Block.Body.ProposerSlashings {
		b.ProposerSlashings[i] = &types.ProposerSlashing{
			ProposerIndex: uint64(proposerSlashing.Header_1.Header.ProposerIndex),
			Header1: &types.Block{
				Slot:       uint64(proposerSlashing.Header_1.Header.Slot),
				ParentRoot: proposerSlashing.Header_1.Header.ParentRoot,
				StateRoot:  proposerSlashing.Header_1.Header.StateRoot,
				Signature:  proposerSlashing.Header_1.Signature,
				BodyRoot:   proposerSlashing.Header_1.Header.BodyRoot,
			},
			Header2: &types.Block{
				Slot:       uint64(proposerSlashing.Header_2.Header.Slot),
				ParentRoot: proposerSlashing.Header_2.Header.ParentRoot,
				StateRoot:  proposerSlashing.Header_2.Header.StateRoot,
				Signature:  proposerSlashing.Header_2.Signature,
				BodyRoot:   proposerSlashing.Header_2.Header.BodyRoot,
			},
		}
	}

	// AttesterSlashings
	for i, attesterSlashing := range blk.Block.Body.AttesterSlashings {
		b.AttesterSlashings[i] = &types.AttesterSlashing{
			Attestation1: &types.IndexedAttestation{
				Data: &types.AttestationData{
					Slot:            uint64(attesterSlashing.Attestation_1.Data.Slot),
					CommitteeIndex:  uint64(attesterSlashing.Attestation_1.Data.CommitteeIndex),
					BeaconBlockRoot: attesterSlashing.Attestation_1.Data.BeaconBlockRoot,
					Source: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_1.Data.Source.Epoch),
						Root:  attesterSlashing.Attestation_1.Data.Source.Root,
					},
					Target: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_1.Data.Target.Epoch),
						Root:  attesterSlashing.Attestation_1.Data.Target.Root,
					},
				},
				Signature:        attesterSlashing.Attestation_1.Signature,
				AttestingIndices: attesterSlashing.Attestation_1.AttestingIndices,
			},
			Attestation2: &types.IndexedAttestation{
				Data: &types.AttestationData{
					Slot:            uint64(attesterSlashing.Attestation_2.Data.Slot),
					CommitteeIndex:  uint64(attesterSlashing.Attestation_2.Data.CommitteeIndex),
					BeaconBlockRoot: attesterSlashing.Attestation_2.Data.BeaconBlockRoot,
					Source: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_2.Data.Source.Epoch),
						Root:  attesterSlashing.Attestation_2.Data.Source.Root,
					},
					Target: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation_2.Data.Target.Epoch),
						Root:  attesterSlashing.Attestation_2.Data.Target.Root,
					},
				},
				Signature:        attesterSlashing.Attestation_2.Signature,
				AttestingIndices: attesterSlashing.Attestation_2.AttestingIndices,
			},
		}
	}

	// Attestations
	for i, attestation := range blk.Block.Body.Attestations {
		a := &types.Attestation{
			AggregationBits: attestation.AggregationBits,
			Data: &types.AttestationData{
				Slot:            uint64(attestation.Data.Slot),
				CommitteeIndex:  uint64(attestation.Data.CommitteeIndex),
				BeaconBlockRoot: attestation.Data.BeaconBlockRoot,
				Source: &types.Checkpoint{
					Epoch: uint64(attestation.Data.Source.Epoch),
					Root:  attestation.Data.Source.Root,
				},
				Target: &types.Checkpoint{
					Epoch: uint64(attestation.Data.Target.Epoch),
					Root:  attestation.Data.Target.Root,
				},
			},
			Signature: attestation.Signature,
		}

		aggregationBits := bitfield.Bitlist(a.AggregationBits)
		assignments, err := pc.GetEpochAssignments(a.Data.Slot / utils.Config.Chain.Config.SlotsPerEpoch)
		if err != nil {
			return nil, fmt.Errorf("error receiving epoch assignment for epoch %v: %v", a.Data.Slot/utils.Config.Chain.Config.SlotsPerEpoch, err)
		}

		a.Attesters = make([]uint64, 0)
		for i := uint64(0); i < aggregationBits.Len(); i++ {
			if aggregationBits.BitAt(i) {
				validator, found := assignments.AttestorAssignments[utils.FormatAttestorAssignmentKey(a.Data.Slot, a.Data.CommitteeIndex, i)]
				if !found { // This should never happen!
					validator = 0
					logger.Errorf("error retrieving assigned validator for attestation %v of block %v for slot %v committee index %v member index %v", i, b.Slot, a.Data.Slot, a.Data.CommitteeIndex, i)
				}
				a.Attesters = append(a.Attesters, validator)
			}
		}

		b.Attestations[i] = a
	}

	// Deposits
	for i, deposit := range blk.Block.Body.Deposits {
		b.Deposits[i] = &types.Deposit{
			Proof:                 deposit.Proof,
			PublicKey:             deposit.Data.PublicKey,
			WithdrawalCredentials: deposit.Data.WithdrawalCredentials,
			Amount:                deposit.Data.Amount,
			Signature:             deposit.Data.Signature,
		}
	}

	// VoluntaryExits
	for i, voluntaryExit := range blk.Block.Body.VoluntaryExits {
		b.VoluntaryExits[i] = &types.VoluntaryExit{
			Epoch:          uint64(voluntaryExit.Exit.Epoch),
			ValidatorIndex: uint64(voluntaryExit.Exit.ValidatorIndex),
			Signature:      voluntaryExit.Signature,
		}
	}
	// SignedBLSToExecutionChange
	for i, blsToExec := range blk.Block.Body.BlsToExecutionChanges {
		b.SignedBLSToExecutionChange[i] = &types.SignedBLSToExecutionChange{
			Message: types.BLSToExecutionChange{
				Validatorindex: uint64(blsToExec.Message.ValidatorIndex),
				BlsPubkey:      blsToExec.Message.FromBlsPubkey,
				Address:        blsToExec.Message.ToExecutionAddress,
			},
			Signature: blsToExec.Signature,
		}
	}
	return b, nil
}

// GetValidatorParticipation will get the validator participation from Prysm client
func (pc *PrysmClient) GetValidatorParticipation(epoch uint64) (*types.ValidatorParticipation, error) {
	validatorParticipationRequest := &ethpb.GetValidatorParticipationRequest{QueryFilter: &ethpb.GetValidatorParticipationRequest_Epoch{Epoch: eth2types.Epoch(epoch)}}
	if epoch == 0 {
		validatorParticipationRequest.QueryFilter = &ethpb.GetValidatorParticipationRequest_Genesis{Genesis: true}
	}
	epochParticipationStatistics, err := pc.client.GetValidatorParticipation(context.Background(), validatorParticipationRequest)
	if err != nil {
		logger.Printf("error retrieving epoch participation statistics: %v", err)
		return &types.ValidatorParticipation{
			Epoch:                   epoch,
			Finalized:               false,
			GlobalParticipationRate: 0,
			VotedEther:              0,
			EligibleEther:           0,
		}, nil
	}
	return &types.ValidatorParticipation{
		Epoch:                   epoch,
		Finalized:               epochParticipationStatistics.Finalized,
		GlobalParticipationRate: float32(epochParticipationStatistics.Participation.PreviousEpochTargetAttestingGwei) / float32(epochParticipationStatistics.Participation.PreviousEpochActiveGwei),
		VotedEther:              epochParticipationStatistics.Participation.PreviousEpochTargetAttestingGwei,
		EligibleEther:           epochParticipationStatistics.Participation.PreviousEpochActiveGwei,
	}, nil
}

func (pc *PrysmClient) GetFinalityCheckpoints(epoch uint64) (*types.FinalityCheckpoints, error) {
	// finalityResp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%s/finality_checkpoints", lc.endpoint, id))
	// if err != nil {
	// 	return nil, fmt.Errorf("error retrieving finality checkpoints of head: %v", err)
	// }
	return nil, fmt.Errorf("not implemented yet")
}

func (pc *PrysmClient) GetSyncCommittee(stateID string, epoch uint64) (*StandardSyncCommittee, error) {
	syncCommitteesResp, err := pc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%s/sync_committees?epoch=%d", pc.endpoint, stateID, epoch))
	if err != nil {
		return nil, fmt.Errorf("error retrieving sync_committees for epoch %v (state: %v): %w", epoch, stateID, err)
	}
	var parsedSyncCommittees StandardSyncCommitteesResponse
	err = json.Unmarshal(syncCommitteesResp, &parsedSyncCommittees)
	if err != nil {
		return nil, fmt.Errorf("error parsing sync_committees data for epoch %v (state: %v): %w", epoch, stateID, err)
	}
	return &parsedSyncCommittees.Data, nil
}

// GetSlotData will get the slot data from a Prysm client
func (pc *PrysmClient) GetSlotData(block *types.Block) (*types.SlotData, error) {
	if block.Slot > PrysmLatestHeadSlot {
		PrysmLatestHeadSlot = block.Slot
	}

	wg := &sync.WaitGroup{}
	var err error

	slot := block.Slot
	epoch := utils.EpochOfSlot(slot)
	data := &types.SlotData{}
	data.Epoch = epoch
	data.Slot = slot

	validatorsResp, err := pc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%d/validators", pc.endpoint, slot))
	if err != nil {
		return nil, fmt.Errorf("error retrieving validators for slot %v: %v", slot, err)
	}
	var parsedValidators StandardValidatorsResponse
	err = json.Unmarshal(validatorsResp, &parsedValidators)
	if err != nil {
		return nil, fmt.Errorf("error parsing epoch validators: %v", err)
	}

	slot1d := int64(slot) - 7200
	slot7d := int64(slot) - 7200*7
	slot31d := int64(slot) - 7200*31

	if slot1d < 0 {
		slot1d = 0
	}
	if slot7d < 0 {
		slot7d = 0
	}
	if slot31d < 0 {
		slot31d = 0
	}

	var validatorBalances1d map[uint64]uint64
	var validatorBalances7d map[uint64]uint64
	var validatorBalances31d map[uint64]uint64
	var validatorWithdrawal map[uint64]uint64
	var validatorWithdrawal1d map[uint64]uint64
	var validatorWithdrawal7d map[uint64]uint64
	var validatorWithdrawal31d map[uint64]uint64

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorWithdrawal, err = db.GetAllValidatorTotalWithdrawals(uint64(slot))
		if err != nil {
			logrus.Errorf("error retrieving validator total withdrawal for slot %v : %v", slot, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for slot %v (1d) took %v", len(parsedValidators.Data), slot1d, time.Since(start))
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances1d, err = pc.GetBalancesForSlot(slot1d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for slot %v (1d): %v", slot1d, err)
			return
		}
		validatorWithdrawal1d, err = db.GetAllValidatorTotalWithdrawals(uint64(slot1d))
		if err != nil {
			logrus.Errorf("error retrieving validator total withdrawal for slot %v (1d): %v", slot1d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for slot %v (1d) took %v", len(parsedValidators.Data), slot1d, time.Since(start))
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances7d, err = pc.GetBalancesForSlot(slot7d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for slot %v (7d): %v", slot7d, err)
			return
		}
		validatorWithdrawal7d, err = db.GetAllValidatorTotalWithdrawals(uint64(slot7d))
		if err != nil {
			logrus.Errorf("error retrieving validator total withdrawal for slot %v (7d): %v", slot7d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for slot %v (7d) took %v", len(parsedValidators.Data), slot7d, time.Since(start))
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances31d, err = pc.GetBalancesForSlot(slot31d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for slot %v (31d): %v", slot31d, err)
			return
		}
		validatorWithdrawal31d, err = db.GetAllValidatorTotalWithdrawals(uint64(slot31d))
		if err != nil {
			logrus.Errorf("error retrieving validator total withdrawal for slot %v (31d): %v", slot31d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for slot %v (31d) took %v", len(parsedValidators.Data), slot31d, time.Since(start))
	}()
	wg.Wait()

	// Retrieve a block for the slot
	data.Blocks = make(map[uint64]map[string]*types.Block)
	if data.Blocks[block.Slot] == nil {
		data.Blocks[block.Slot] = make(map[string]*types.Block)
	}
	data.Blocks[block.Slot][fmt.Sprintf("%x", block.BlockRoot)] = block
	logger.Printf("retrieved a block for slot %v", slot)

	if block.Slot > PrysmLatestHeadSlot {
		for _, b := range data.Blocks[block.Slot] {
			if payload := b.ExecutionPayload; payload != nil && payload.Withdrawals != nil {
				for _, wd := range payload.Withdrawals {
					value, exists := validatorWithdrawal[wd.ValidatorIndex]
					if exists {
						validatorWithdrawal[wd.ValidatorIndex] = value + wd.Amount
					} else {
						validatorWithdrawal[wd.ValidatorIndex] = wd.Amount
					}
				}
			}
		}
	}

	// Retrieve the validator set for the slot
	data.Validators = make([]*types.Validator, 0)

	for _, validator := range parsedValidators.Data {
		data.Validators = append(data.Validators, &types.Validator{
			Index:                      uint64(validator.Index),
			PublicKey:                  utils.MustParseHex(validator.Validator.Pubkey),
			WithdrawalCredentials:      utils.MustParseHex(validator.Validator.WithdrawalCredentials),
			Balance:                    uint64(validator.Balance),
			EffectiveBalance:           uint64(validator.Validator.EffectiveBalance),
			Slashed:                    validator.Validator.Slashed,
			ActivationEligibilityEpoch: uint64(validator.Validator.ActivationEligibilityEpoch),
			ActivationEpoch:            uint64(validator.Validator.ActivationEpoch),
			ExitEpoch:                  uint64(validator.Validator.ExitEpoch),
			WithdrawableEpoch:          uint64(validator.Validator.WithdrawableEpoch),
			Balance1d:                  validatorBalances1d[uint64(validator.Index)],
			Balance7d:                  validatorBalances7d[uint64(validator.Index)],
			Balance31d:                 validatorBalances31d[uint64(validator.Index)],
			Withdrawal:                 validatorWithdrawal[uint64(validator.Index)],
			Withdrawal1d:               validatorWithdrawal1d[uint64(validator.Index)],
			Withdrawal7d:               validatorWithdrawal7d[uint64(validator.Index)],
			Withdrawal31d:              validatorWithdrawal31d[uint64(validator.Index)],
			Status:                     validator.Status,
		})
	}

	logger.Printf("retrieved data for %v validators for slot %v", len(data.Validators), slot)

	return data, nil
}

func (pc *PrysmClient) GetBalancesForSlot(slot int64) (map[uint64]uint64, error) {
	if slot < 0 {
		slot = 0
	}

	var err error

	validatorBalances := make(map[uint64]uint64)

	resp, err := pc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%d/validator_balances", pc.endpoint, slot))
	if err != nil {
		return validatorBalances, err
	}

	var parsedResponse StandardValidatorBalancesResponse
	err = json.Unmarshal(resp, &parsedResponse)
	if err != nil {
		return nil, fmt.Errorf("error parsing response for validator_balances")
	}

	for _, b := range parsedResponse.Data {
		validatorBalances[uint64(b.Index)] = uint64(b.Balance)
	}

	return validatorBalances, nil
}

func (pc *PrysmClient) get(url string) ([]byte, error) {
	// t0 := time.Now()
	// defer func() { fmt.Println(url, time.Since(t0)) }()
	client := &http.Client{Timeout: time.Second * 120}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, errors.New("not found 404")
		}
		return nil, fmt.Errorf("url: %v, error-response: %s", url, data)
	}
	return data, err
}
