package watchtower

import (
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/rocket-pool/rocketpool-go/minipool"
	"github.com/rocket-pool/rocketpool-go/rocketpool"
	rptypes "github.com/rocket-pool/rocketpool-go/types"
	"github.com/rocket-pool/rocketpool-go/utils/eth"
	"github.com/urfave/cli"

	"github.com/rocket-pool/smartnode/shared/services"
	"github.com/rocket-pool/smartnode/shared/services/config"
	"github.com/rocket-pool/smartnode/shared/services/state"
	"github.com/rocket-pool/smartnode/shared/services/wallet"
	"github.com/rocket-pool/smartnode/shared/utils/api"
	"github.com/rocket-pool/smartnode/shared/utils/log"
)

// Settings
const MinipoolStatusBatchSize = 20

// Dissolve timed out minipools task
type dissolveTimedOutMinipools struct {
	c   *cli.Context
	log log.ColorLogger
	cfg *config.RocketPoolConfig
	w   *wallet.Wallet
	ec  rocketpool.ExecutionClient
	rp  *rocketpool.RocketPool
	m   *state.NetworkStateManager
	s   *state.NetworkState
}

// Create dissolve timed out minipools task
func newDissolveTimedOutMinipools(c *cli.Context, logger log.ColorLogger, m *state.NetworkStateManager) (*dissolveTimedOutMinipools, error) {

	// Get services
	cfg, err := services.GetConfig(c)
	if err != nil {
		return nil, err
	}
	w, err := services.GetWallet(c)
	if err != nil {
		return nil, err
	}
	ec, err := services.GetEthClient(c)
	if err != nil {
		return nil, err
	}
	rp, err := services.GetRocketPool(c)
	if err != nil {
		return nil, err
	}

	// Return task
	return &dissolveTimedOutMinipools{
		c:   c,
		log: logger,
		cfg: cfg,
		w:   w,
		ec:  ec,
		rp:  rp,
		m:   m,
	}, nil

}

// Dissolve timed out minipools
func (t *dissolveTimedOutMinipools) run(isAtlasDeployed bool) error {

	// Wait for eth client to sync
	if err := services.WaitEthClientSynced(t.c, true); err != nil {
		return err
	}

	// Get the latest state
	t.s = t.m.GetLatestState()
	opts := &bind.CallOpts{
		BlockNumber: big.NewInt(0).SetUint64(t.s.ElBlockNumber),
	}

	// Log
	t.log.Println("Checking for timed out minipools to dissolve...")

	// Get timed out minipools
	minipools, err := t.getTimedOutMinipools(opts)
	if err != nil {
		return err
	}
	if len(minipools) == 0 {
		return nil
	}

	// Log
	t.log.Printlnf("%d minipool(s) have timed out and will be dissolved...", len(minipools))

	// Dissolve minipools
	for _, mp := range minipools {
		if err := t.dissolveMinipool(mp); err != nil {
			t.log.Println(fmt.Errorf("Could not dissolve minipool %s: %w", mp.GetAddress().Hex(), err))
		}
	}

	// Return
	return nil

}

// Get timed out minipools
func (t *dissolveTimedOutMinipools) getTimedOutMinipools(opts *bind.CallOpts) ([]minipool.Minipool, error) {

	timedOutMinipools := []minipool.Minipool{}
	genesisTime := time.Unix(int64(t.s.BeaconConfig.GenesisTime), 0)
	secondsSinceGenesis := time.Duration(t.s.BeaconSlotNumber*t.s.BeaconConfig.SecondsPerSlot) * time.Second
	blockTime := genesisTime.Add(secondsSinceGenesis)

	// Filter minipools by status
	launchTimeoutBig := t.s.NetworkDetails.MinipoolLaunchTimeout
	launchTimeout := time.Duration(launchTimeoutBig.Uint64()) * time.Second
	for _, mpd := range t.s.MinipoolDetails {
		statusTime := time.Unix(mpd.StatusBlock.Int64(), 0)
		if mpd.Status == rptypes.Prelaunch && blockTime.Sub(statusTime) >= launchTimeout {
			mp, err := minipool.NewMinipoolFromVersion(t.rp, mpd.MinipoolAddress, mpd.Version, opts)
			if err != nil {
				return nil, fmt.Errorf("error creating binding for minipool %s: %w", mpd.MinipoolAddress.Hex(), err)
			}
			timedOutMinipools = append(timedOutMinipools, mp)
		}
	}

	// Return
	return timedOutMinipools, nil

}

// Dissolve a minipool
func (t *dissolveTimedOutMinipools) dissolveMinipool(mp minipool.Minipool) error {

	// Log
	t.log.Printlnf("Dissolving minipool %s...", mp.GetAddress().Hex())

	// Get transactor
	opts, err := t.w.GetNodeAccountTransactor()
	if err != nil {
		return err
	}

	// Get the gas limit
	gasInfo, err := mp.EstimateDissolveGas(opts)
	if err != nil {
		return fmt.Errorf("Could not estimate the gas required to dissolve the minipool: %w", err)
	}

	// Print the gas info
	maxFee := eth.GweiToWei(WatchtowerMaxFee)
	if !api.PrintAndCheckGasInfo(gasInfo, false, 0, t.log, maxFee, 0) {
		return nil
	}

	// Set the gas settings
	opts.GasFeeCap = maxFee
	opts.GasTipCap = eth.GweiToWei(WatchtowerMaxPriorityFee)
	opts.GasLimit = gasInfo.SafeGasLimit

	// Dissolve
	hash, err := mp.Dissolve(opts)
	if err != nil {
		return err
	}

	// Print TX info and wait for it to be included in a block
	err = api.PrintAndWaitForTransaction(t.cfg, hash, t.rp.Client, t.log)
	if err != nil {
		return err
	}

	// Log
	t.log.Printlnf("Successfully dissolved minipool %s.", mp.GetAddress().Hex())

	// Return
	return nil

}
