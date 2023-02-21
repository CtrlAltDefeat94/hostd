package wallet

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"

	"gitlab.com/NebulousLabs/encoding"
	"go.sia.tech/core/consensus"
	"go.sia.tech/core/types"
	"go.sia.tech/siad/modules"
	stypes "go.sia.tech/siad/types"
)

func convertToSiad(core types.EncoderTo, siad encoding.SiaUnmarshaler) {
	var buf bytes.Buffer
	e := types.NewEncoder(&buf)
	core.EncodeTo(e)
	e.Flush()
	if err := siad.UnmarshalSia(&buf); err != nil {
		panic(err)
	}
}

func convertToCore(siad encoding.SiaMarshaler, core types.DecoderFrom) {
	var buf bytes.Buffer
	siad.MarshalSia(&buf)
	d := types.NewBufDecoder(buf.Bytes())
	core.DecodeFrom(d)
	if d.Err() != nil {
		panic(d.Err())
	}
}

// transaction sources indicate the source of a transaction. Transactions can
// either be created by sending Siacoins between unlock hashes or they can be
// created by consensus (e.g. a miner payout, a siafund claim, or a contract).
const (
	TxnSourceTransaction  TransactionSource = "transaction"
	TxnSourceMinerPayout  TransactionSource = "minerPayout"
	TxnSourceSiafundClaim TransactionSource = "siafundClaim"
	TxnSourceContract     TransactionSource = "contract"
)

type (
	// A TransactionSource is a string indicating the source of a transaction.
	TransactionSource string

	// A ChainManager manages the current state of the blockchain.
	ChainManager interface {
		TipState() consensus.State
		BlockAtHeight(height uint64) (types.Block, bool)
	}

	// A SiacoinElement is a SiacoinOutput along with its ID.
	SiacoinElement struct {
		types.SiacoinOutput
		ID types.SiacoinOutputID
	}

	// A Transaction is an on-chain transaction relevant to a particular wallet,
	// paired with useful metadata.
	Transaction struct {
		ID          types.TransactionID `json:"id"`
		Index       types.ChainIndex    `json:"index"`
		Transaction types.Transaction   `json:"transaction"`
		Inflow      types.Currency      `json:"inflow"`
		Outflow     types.Currency      `json:"outflow"`
		Source      TransactionSource   `json:"source"`
		Timestamp   time.Time           `json:"timestamp"`
	}

	// A SingleAddressWallet is a hot wallet that manages the outputs controlled by
	// a single address.
	SingleAddressWallet struct {
		priv  types.PrivateKey
		addr  types.Address
		cm    ChainManager
		store SingleAddressStore

		mu sync.Mutex // protects the following fields
		// txnsets maps a transaction set to its SiacoinOutputIDs.
		txnsets map[modules.TransactionSetID][]types.SiacoinOutputID
		// tpool is a set of siacoin output IDs that are currently in the
		// transaction pool.
		tpool map[types.SiacoinOutputID]bool
		// locked is a set of siacoin output IDs locked by FundTransaction. They
		// will be released either by calling Release for unused transactions or
		// being confirmed in a block.
		locked map[types.SiacoinOutputID]bool
	}

	// An UpdateTransaction atomically updates the wallet store
	UpdateTransaction interface {
		AddSiacoinElement(SiacoinElement) error
		RemoveSiacoinElement(types.SiacoinOutputID) error
		AddTransaction(Transaction) error
		RevertBlock(types.BlockID) error
	}

	// A SingleAddressStore stores the state of a single-address wallet.
	// Implementations are assumed to be thread safe.
	SingleAddressStore interface {
		LastWalletChange() (modules.ConsensusChangeID, error)

		UnspentSiacoinElements() ([]SiacoinElement, error)
		// Transactions returns a paginated list of transactions ordered by
		// block height, descending. If no more transactions are available,
		// (nil, nil) should be returned.
		Transactions(limit, offset int) ([]Transaction, error)
		// TransactionCount returns the total number of transactions in the
		// wallet.
		TransactionCount() (uint64, error)

		UpdateWallet(modules.ConsensusChangeID, func(UpdateTransaction) error) error
	}
)

// EncodeTo implements types.EncoderTo.
func (txn Transaction) EncodeTo(e *types.Encoder) {
	txn.ID.EncodeTo(e)
	txn.Index.EncodeTo(e)
	txn.Transaction.EncodeTo(e)
	txn.Inflow.EncodeTo(e)
	txn.Outflow.EncodeTo(e)
	e.WriteString(string(txn.Source))
	e.WriteTime(txn.Timestamp)
}

// DecodeFrom implements types.DecoderFrom.
func (txn *Transaction) DecodeFrom(d *types.Decoder) {
	txn.ID.DecodeFrom(d)
	txn.Index.DecodeFrom(d)
	txn.Transaction.DecodeFrom(d)
	txn.Inflow.DecodeFrom(d)
	txn.Outflow.DecodeFrom(d)
	txn.Source = TransactionSource(d.ReadString())
	txn.Timestamp = d.ReadTime()
}

func transactionIsRelevant(txn types.Transaction, addr types.Address) bool {
	for i := range txn.SiacoinInputs {
		if txn.SiacoinInputs[i].UnlockConditions.UnlockHash() == addr {
			return true
		}
	}
	for i := range txn.SiacoinOutputs {
		if txn.SiacoinOutputs[i].Address == addr {
			return true
		}
	}
	for i := range txn.SiafundInputs {
		if txn.SiafundInputs[i].UnlockConditions.UnlockHash() == addr {
			return true
		}
		if txn.SiafundInputs[i].ClaimAddress == addr {
			return true
		}
	}
	for i := range txn.SiafundOutputs {
		if txn.SiafundOutputs[i].Address == addr {
			return true
		}
	}
	for i := range txn.FileContracts {
		for _, sco := range txn.FileContracts[i].ValidProofOutputs {
			if sco.Address == addr {
				return true
			}
		}
		for _, sco := range txn.FileContracts[i].MissedProofOutputs {
			if sco.Address == addr {
				return true
			}
		}
	}
	for i := range txn.FileContractRevisions {
		for _, sco := range txn.FileContractRevisions[i].ValidProofOutputs {
			if sco.Address == addr {
				return true
			}
		}
		for _, sco := range txn.FileContractRevisions[i].MissedProofOutputs {
			if sco.Address == addr {
				return true
			}
		}
	}
	return false
}

// Close closes the wallet
func (sw *SingleAddressWallet) Close() error {
	return nil
}

// Address returns the address of the wallet.
func (sw *SingleAddressWallet) Address() types.Address {
	return sw.addr
}

// Balance returns the balance of the wallet.
func (sw *SingleAddressWallet) Balance() (spendable, confirmed types.Currency, err error) {
	outputs, err := sw.store.UnspentSiacoinElements()
	if err != nil {
		return types.Currency{}, types.Currency{}, fmt.Errorf("failed to get unspent outputs: %w", err)
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	for _, sco := range outputs {
		confirmed = confirmed.Add(sco.Value)
		if !sw.locked[sco.ID] || sw.tpool[sco.ID] {
			spendable = spendable.Add(sco.Value)
		}
	}
	return
}

// Transactions returns a paginated list of transactions, ordered by block
// height descending. If no more transactions are available, (nil, nil) is
// returned.
func (sw *SingleAddressWallet) Transactions(limit, offset int) ([]Transaction, error) {
	return sw.store.Transactions(limit, offset)
}

// TransactionCount returns the total number of transactions in the wallet.
func (sw *SingleAddressWallet) TransactionCount() (uint64, error) {
	return sw.store.TransactionCount()
}

// FundTransaction adds siacoin inputs worth at least amount to the provided
// transaction. If necessary, a change output will also be added. The inputs
// will not be available to future calls to FundTransaction unless ReleaseInputs
// is called.
func (sw *SingleAddressWallet) FundTransaction(txn *types.Transaction, amount types.Currency) ([]types.Hash256, func(), error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if amount.IsZero() {
		return nil, nil, nil
	}

	utxos, err := sw.store.UnspentSiacoinElements()
	if err != nil {
		return nil, nil, err
	}
	var inputSum types.Currency
	var fundingElements []SiacoinElement
	for _, sce := range utxos {
		if sw.locked[sce.ID] || sw.tpool[sce.ID] {
			continue
		}
		fundingElements = append(fundingElements, sce)
		inputSum = inputSum.Add(sce.Value)
		if inputSum.Cmp(amount) >= 0 {
			break
		}
	}
	if inputSum.Cmp(amount) < 0 {
		return nil, nil, errors.New("insufficient balance")
	} else if inputSum.Cmp(amount) > 0 {
		txn.SiacoinOutputs = append(txn.SiacoinOutputs, types.SiacoinOutput{
			Value:   inputSum.Sub(amount),
			Address: sw.addr,
		})
	}

	toSign := make([]types.Hash256, len(fundingElements))
	for i, sce := range fundingElements {
		txn.SiacoinInputs = append(txn.SiacoinInputs, types.SiacoinInput{
			ParentID:         types.SiacoinOutputID(sce.ID),
			UnlockConditions: StandardUnlockConditions(sw.priv.PublicKey()),
		})
		toSign[i] = types.Hash256(sce.ID)
		sw.locked[sce.ID] = true
	}

	release := func() {
		sw.mu.Lock()
		defer sw.mu.Unlock()
		for _, id := range toSign {
			delete(sw.locked, types.SiacoinOutputID(id))
		}
	}

	return toSign, release, nil
}

// SignTransaction adds a signature to each of the specified inputs.
func (sw *SingleAddressWallet) SignTransaction(cs consensus.State, txn *types.Transaction, toSign []types.Hash256, cf types.CoveredFields) error {
	// NOTE: siad uses different hardfork heights when -tags=testing is set,
	// so we have to alter cs accordingly.
	// TODO: remove this
	switch {
	case cs.Index.Height >= uint64(stypes.FoundationHardforkHeight):
		cs.Index.Height = 298000
	case cs.Index.Height >= uint64(stypes.ASICHardforkHeight):
		cs.Index.Height = 179000
	}

	for _, id := range toSign {
		var h types.Hash256
		if cf.WholeTransaction {
			h = cs.WholeSigHash(*txn, id, 0, 0, cf.Signatures)
		} else {
			h = cs.PartialSigHash(*txn, cf)
		}
		sig := sw.priv.SignHash(h)
		txn.Signatures = append(txn.Signatures, types.TransactionSignature{
			ParentID:       id,
			CoveredFields:  cf,
			PublicKeyIndex: 0,
			Signature:      sig[:],
		})
	}
	return nil
}

// ReceiveUpdatedUnconfirmedTransactions implements modules.TransactionPoolSubscriber.
func (sw *SingleAddressWallet) ReceiveUpdatedUnconfirmedTransactions(diff *modules.TransactionPoolDiff) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	for _, txnsetID := range diff.RevertedTransactions {
		for _, outputID := range sw.txnsets[txnsetID] {
			delete(sw.tpool, outputID)
		}
		delete(sw.txnsets, txnsetID)
	}

	for _, txnset := range diff.AppliedTransactions {
		var txnsetOutputs []types.SiacoinOutputID
		for _, txn := range txnset.Transactions {
			for _, sci := range txn.SiacoinInputs {
				if types.Address(sci.UnlockConditions.UnlockHash()) == sw.addr {
					sw.tpool[types.SiacoinOutputID(sci.ParentID)] = true
					txnsetOutputs = append(txnsetOutputs, types.SiacoinOutputID(sci.ParentID))
				}
			}
		}
		if len(txnsetOutputs) > 0 {
			sw.txnsets[txnset.ID] = txnsetOutputs
		}
	}
}

// ProcessConsensusChange implements modules.ConsensusSetSubscriber.
func (sw *SingleAddressWallet) ProcessConsensusChange(cc modules.ConsensusChange) {
	// create payout transactions for each matured siacoin output. Each diff
	// should correspond to an applied block. This is done outside of the
	// database transaction to reduce lock contention.
	appliedPayoutTxns := make([][]Transaction, len(cc.AppliedDiffs))
	// calculate the block height of the first applied diff
	blockHeight := uint64(cc.BlockHeight) - uint64(len(cc.AppliedBlocks)) + 1
	for i := 0; i < len(cc.AppliedDiffs); i, blockHeight = i+1, blockHeight+1 {
		var block types.Block
		convertToCore(cc.AppliedBlocks[i], &block)
		diff := cc.AppliedDiffs[i]
		index := types.ChainIndex{
			ID:     block.ID(),
			Height: blockHeight,
		}

		// determine the source of each delayed output
		delayedOutputSources := make(map[types.SiacoinOutputID]TransactionSource)
		if blockHeight > uint64(stypes.MaturityDelay) {
			// get the block that has matured
			matureBlock, ok := sw.cm.BlockAtHeight(blockHeight - uint64(stypes.MaturityDelay))
			if !ok {
				panic(fmt.Errorf("failed to get mature block at height %v", blockHeight-uint64(stypes.MaturityDelay)))
			}
			matureID := matureBlock.ID()
			for i := range matureBlock.MinerPayouts {
				delayedOutputSources[matureID.MinerOutputID(i)] = TxnSourceMinerPayout
			}
			for _, txn := range matureBlock.Transactions {
				for _, output := range txn.SiafundInputs {
					delayedOutputSources[output.ParentID.ClaimOutputID()] = TxnSourceSiafundClaim
				}
			}
		}

		for _, dsco := range diff.DelayedSiacoinOutputDiffs {
			// if a delayed output is reverted in an applied diff, the
			// output has matured -- add a payout transaction.
			if types.Address(dsco.SiacoinOutput.UnlockHash) != sw.addr || dsco.Direction != modules.DiffRevert {
				continue
			}
			// contract payouts are harder to identify, any unknown output
			// ID is assumed to be a contract payout.
			var source TransactionSource
			if s, ok := delayedOutputSources[types.SiacoinOutputID(dsco.ID)]; ok {
				source = s
			} else {
				source = TxnSourceContract
			}
			// append the payout transaction to the diff
			var utxo types.SiacoinOutput
			convertToCore(dsco.SiacoinOutput, &utxo)
			sce := SiacoinElement{
				ID:            types.SiacoinOutputID(dsco.ID),
				SiacoinOutput: utxo,
			}
			appliedPayoutTxns[i] = append(appliedPayoutTxns[i], payoutTransaction(sce, index, source, block.Timestamp))
		}
	}

	// begin a database transaction to update the wallet state
	err := sw.store.UpdateWallet(cc.ID, func(tx UpdateTransaction) error {
		// add new siacoin outputs and remove spent or reverted siacoin outputs
		for _, diff := range cc.SiacoinOutputDiffs {
			if types.Address(diff.SiacoinOutput.UnlockHash) != sw.addr {
				continue
			}
			if diff.Direction == modules.DiffApply {
				var sco types.SiacoinOutput
				convertToCore(diff.SiacoinOutput, &sco)
				err := tx.AddSiacoinElement(SiacoinElement{
					SiacoinOutput: sco,
					ID:            types.SiacoinOutputID(diff.ID),
				})
				if err != nil {
					return fmt.Errorf("failed to add siacoin element %v: %w", diff.ID, err)
				}
			} else {
				err := tx.RemoveSiacoinElement(types.SiacoinOutputID(diff.ID))
				if err != nil {
					return fmt.Errorf("failed to remove siacoin element %v: %w", diff.ID, err)
				}
				// release the locks on the spent outputs
				sw.mu.Lock()
				delete(sw.locked, types.SiacoinOutputID(diff.ID))
				delete(sw.tpool, types.SiacoinOutputID(diff.ID))
				sw.mu.Unlock()
			}
		}

		// revert blocks -- will also revert all transactions and payout transactions
		for _, reverted := range cc.RevertedBlocks {
			blockID := types.BlockID(reverted.ID())
			if err := tx.RevertBlock(blockID); err != nil {
				return fmt.Errorf("failed to revert block %v: %w", blockID, err)
			}
		}

		// calculate the block height of the first applied block
		blockHeight = uint64(cc.BlockHeight) - uint64(len(cc.AppliedBlocks)) + 1
		// apply transactions
		for i := 0; i < len(cc.AppliedBlocks); i, blockHeight = i+1, blockHeight+1 {
			var block types.Block
			convertToCore(cc.AppliedBlocks[i], &block)
			index := types.ChainIndex{
				ID:     block.ID(),
				Height: blockHeight,
			}

			// apply actual transactions -- only relevant transactions should be
			// added to the database
			for _, txn := range block.Transactions {
				if !transactionIsRelevant(txn, sw.addr) {
					continue
				}
				var inflow, outflow types.Currency
				for _, out := range txn.SiacoinOutputs {
					if out.Address == sw.addr {
						inflow = inflow.Add(out.Value)
					}
				}
				for _, in := range txn.SiacoinInputs {
					if in.UnlockConditions.UnlockHash() == sw.addr {
						inputValue := types.ZeroCurrency
						outflow = outflow.Add(inputValue)
					}
				}

				err := tx.AddTransaction(Transaction{
					ID:          txn.ID(),
					Index:       index,
					Inflow:      inflow,
					Outflow:     outflow,
					Source:      TxnSourceTransaction,
					Transaction: txn,
					Timestamp:   block.Timestamp,
				})
				if err != nil {
					return fmt.Errorf("failed to add transaction %v: %w", txn.ID(), err)
				}
			}

			// apply payout transactions -- all transactions should be relevant
			// to the wallet
			for _, txn := range appliedPayoutTxns[i] {
				if err := tx.AddTransaction(txn); err != nil {
					return fmt.Errorf("failed to add payout transaction %v: %w", txn.ID, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
}

// payoutTransaction wraps a delayed siacoin output in a transaction for display
// in the wallet.
func payoutTransaction(output SiacoinElement, index types.ChainIndex, source TransactionSource, timestamp time.Time) Transaction {
	return Transaction{
		ID:    types.TransactionID(output.ID),
		Index: index,
		Transaction: types.Transaction{
			SiacoinOutputs: []types.SiacoinOutput{output.SiacoinOutput},
		},
		Inflow:    output.Value,
		Source:    source,
		Timestamp: timestamp,
	}
}

// NewSingleAddressWallet returns a new SingleAddressWallet using the provided private key and store.
func NewSingleAddressWallet(priv types.PrivateKey, cm ChainManager, store SingleAddressStore) *SingleAddressWallet {
	return &SingleAddressWallet{
		priv:    priv,
		addr:    StandardAddress(priv.PublicKey()),
		store:   store,
		locked:  make(map[types.SiacoinOutputID]bool),
		tpool:   make(map[types.SiacoinOutputID]bool),
		txnsets: make(map[modules.TransactionSetID][]types.SiacoinOutputID),
		cm:      cm,
	}
}

// StandardUnlockConditions returns the standard unlock conditions for a single
// Ed25519 key.
func StandardUnlockConditions(pk types.PublicKey) types.UnlockConditions {
	return types.UnlockConditions{
		PublicKeys:         []types.UnlockKey{pk.UnlockKey()},
		SignaturesRequired: 1,
	}
}

// StandardAddress returns the standard address for an Ed25519 key.
func StandardAddress(pk types.PublicKey) types.Address {
	return StandardUnlockConditions(pk).UnlockHash()
}
