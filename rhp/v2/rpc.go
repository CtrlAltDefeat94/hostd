package rhp

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.sia.tech/hostd/host/contracts"
	"go.sia.tech/hostd/host/storage"
	"go.sia.tech/hostd/internal/merkle"
	"go.sia.tech/siad/crypto"
	"go.sia.tech/siad/types"
	"lukechampine.com/frand"
)

type (
	// An rpcError may be sent instead of a response object to any RPC.
	rpcError struct {
		Type        Specifier
		Data        []byte // structure depends on Type
		Description string // human-readable error string
	}

	// An rpcResponse is a helper type for encoding and decoding RPC responses.
	rpcResponse struct {
		err *rpcError
		obj rpcObject
	}
)

var (
	// ErrTxnMissingContract is returned if the transaction set does not contain
	// any transactions or if the transaction does not contain exactly one
	// contract.
	ErrTxnMissingContract = errors.New("transaction set does not contain a file contract")
	// ErrHostInternalError is returned if the host encountered an error during
	// an RPC that doesn't need to be broadcast to the renter (e.g. insufficient
	// funds).
	ErrHostInternalError = errors.New("unable to form contract")
	// ErrInvalidRenterSignature is returned when a contract's renter signature
	// is invalid.
	ErrInvalidRenterSignature = errors.New("invalid renter signature")

	// ErrContractAlreadyLocked is returned when a renter tries to lock
	// a contract before unlocking the previous one.
	ErrContractAlreadyLocked = errors.New("contract already locked")

	// ErrRenterClosed is returned by (*Transport).ReadID when the renter sends the
	// Transport termination signal.
	ErrRenterClosed = errors.New("renter has terminated Transport")
)

var (
	// Handshake specifiers
	loopEnter = newSpecifier("LoopEnter")
	loopExit  = newSpecifier("LoopExit")

	// RPC ciphers
	cipherChaCha20Poly1305 = newSpecifier("ChaCha20Poly1305")
	cipherNoOverlap        = newSpecifier("NoOverlap")

	// RPC IDs
	rpcFormContractID       = newSpecifier("LoopFormContract")
	rpcLockID               = newSpecifier("LoopLock")
	rpcReadID               = newSpecifier("LoopRead")
	rpcRenewClearContractID = newSpecifier("LoopRenewClear")
	rpcSectorRootsID        = newSpecifier("LoopSectorRoots")
	rpcSettingsID           = newSpecifier("LoopSettings")
	rpcUnlockID             = newSpecifier("LoopUnlock")
	rpcWriteID              = newSpecifier("LoopWrite")

	// Read/Write actions
	rpcWriteActionAppend = newSpecifier("Append")
	rpcWriteActionTrim   = newSpecifier("Trim")
	rpcWriteActionSwap   = newSpecifier("Swap")
	rpcWriteActionUpdate = newSpecifier("Update")

	rpcReadStop = newSpecifier("ReadStop")
)

func (sh *SessionHandler) rpcSettings(s *session) error {
	settings, err := sh.Settings()
	if err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to get host settings: %w", err)
	}
	js, err := json.Marshal(settings)
	if err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to marshal settings: %v", err)
	}
	return s.WriteResponse(&rpcSettingsResponse{
		Settings: js,
	}, 30*time.Second)
}

func (sh *SessionHandler) rpcLock(s *session) error {
	var req rpcLockRequest
	if err := s.ReadRequest(&req, minMessageSize, 30*time.Second); err != nil {
		return err
	}

	// Check if a contract is already locked.
	if s.contract.Revision.ParentID != (types.FileContractID{}) {
		return s.WriteError(ErrContractAlreadyLocked)
	}

	contract, err := sh.contracts.Lock(req.ContractID, time.Duration(req.Timeout)*time.Millisecond)
	if err != nil {
		return s.WriteError(fmt.Errorf("failed to lock contract: %w", err))
	}

	// verify the renter's challenge signature
	if !s.VerifyChallenge(req.Signature, contract.RenterKey()) {
		// unlock the contract since the renter failed the challenge
		sh.contracts.Unlock(contract.Revision.ParentID)
		return s.WriteError(fmt.Errorf("challenge failed: %w", ErrInvalidRenterSignature))
	}

	// set the contract
	s.contract = contract
	s.challenge = frand.Entropy128()
	lockResp := &rpcLockResponse{
		Acquired:     true,
		NewChallenge: s.challenge,
		Revision:     contract.Revision,
		Signatures:   contract.Signatures(),
	}
	// avoid holding lock during network round trip
	if err := s.WriteResponse(lockResp, 30*time.Second); err != nil {
		sh.contracts.Unlock(contract.Revision.ParentID)
		return fmt.Errorf("failed to write lock response: %w", err)
	}
	return nil
}

// rpcUnlock unlocks the contract associated with the session.
func (sh *SessionHandler) rpcUnlock(s *session) error {
	// check if a contract is locked
	if s.contract.Revision.ParentID == (types.FileContractID{}) {
		return nil
	}

	s.contract = contracts.SignedRevision{}
	sh.contracts.Unlock(s.contract.Revision.ParentID)
	return nil
}

// rpcFormContract is an RPC that forms a contract between a renter and the
// host.
func (sh *SessionHandler) rpcFormContract(s *session) error {
	var req rpcFormContractRequest
	if err := s.ReadRequest(&req, 10*minMessageSize, time.Minute); err != nil {
		return err
	}
	formationTxnSet := req.Transactions
	// if the transaction set does not contain any transaction or if the
	// transaction does not contain exactly one file contract, return an error
	if len(formationTxnSet) == 0 || len(formationTxnSet[len(formationTxnSet)-1].FileContracts) != 1 {
		return s.WriteError(ErrTxnMissingContract)
	} else if req.RenterKey.Algorithm != types.SignatureEd25519 {
		return s.WriteError(errors.New("unsupported renter key algorithm"))
	}
	renterPub := req.RenterKey
	// get the host's public key, current block height, and settings
	hostPub := types.SiaPublicKey{
		Algorithm: types.SignatureEd25519,
		Key:       sh.privateKey.Public().(ed25519.PublicKey),
	}
	settings, err := sh.Settings()
	if err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to get host settings: %w", err)
	}
	currentHeight := sh.cm.Tip().Index.Height
	// get the contract from the transaction set
	formationTxn := &formationTxnSet[len(formationTxnSet)-1]

	// validate the contract formation fields. note: the v1 contract type
	// does not contain the public keys or signatures.
	if err := validateContractFormation(formationTxn.FileContracts[0], hostPub, renterPub, currentHeight, settings); err != nil {
		return s.WriteError(fmt.Errorf("contract rejected: validation failed: %w", err))
	}

	// calculate the host's collateral and add the inputs to the transaction
	hostCollateral := formationTxn.FileContracts[0].ValidProofOutputs[1].Value.Sub(settings.ContractPrice)
	renterInputs, renterOutputs := len(formationTxn.SiacoinInputs), len(formationTxn.SiacoinOutputs)
	toSign, discard, err := sh.wallet.FundTransaction(formationTxn, hostCollateral)
	if err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to fund formation transaction: %w", err)
	}
	defer discard()

	// create an initial revision for the contract
	initialRevision := initialRevision(formationTxn, hostPub, renterPub)
	sigHash := hashRevision(initialRevision)
	hostSig := ed25519.Sign(sh.privateKey, sigHash[:])

	// send the host's transaction funding additions to the renter
	hostAdditionsResp := &rpcFormContractAdditions{
		Inputs:  formationTxn.SiacoinInputs[renterInputs:],
		Outputs: formationTxn.SiacoinOutputs[renterOutputs:],
	}
	if err := s.WriteResponse(hostAdditionsResp, 30*time.Second); err != nil {
		return fmt.Errorf("failed to write host additions: %w", err)
	}

	// read the renter's signatures
	var renterSignaturesResp rpcFormContractSignatures
	if err := s.ReadResponse(&renterSignaturesResp, minMessageSize, 30*time.Second); err != nil {
		return fmt.Errorf("failed to read renter signatures: %w", err)
	}
	// validate the renter's initial revision signature
	renterSig := renterSignaturesResp.RevisionSignature.Signature
	if !ed25519.Verify(renterPub.Key, sigHash[:], renterSig) {
		return s.WriteError(ErrInvalidRenterSignature)
	}
	// add the renter's signatures to the transaction and contract revision
	renterTxnSigs := len(renterSignaturesResp.ContractSignatures)
	formationTxn.TransactionSignatures = renterSignaturesResp.ContractSignatures

	// sign and broadcast the formation transaction
	if err = sh.wallet.SignTransaction(formationTxn, toSign, types.FullCoveredFields); err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to sign formation transaction: %w", err)
	} else if err = sh.tpool.AcceptTransactionSet(formationTxnSet); err != nil {
		return s.WriteError(fmt.Errorf("failed to broadcast formation transaction: %w", err))
	}

	signedRevision := contracts.SignedRevision{
		Revision:        initialRevision,
		RenterSignature: renterSig,
		HostSignature:   hostSig,
	}
	if err := sh.contracts.AddContract(signedRevision, formationTxnSet, hostCollateral, currentHeight); err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to add contract to store: %w", err)
	}

	// add the contract fee to the amount spent by the renter
	s.Spend(settings.ContractPrice)
	// log the formation event
	sh.metrics.Report(EventContractFormed{
		SessionUID: s.uid,
		ContractID: formationTxn.FileContractID(0),
		Contract:   initialRevision,
	})

	// send the host signatures to the renter
	hostSignaturesResp := &rpcFormContractSignatures{
		ContractSignatures: formationTxn.TransactionSignatures[renterTxnSigs:],
		RevisionSignature: types.TransactionSignature{
			ParentID:      crypto.Hash(formationTxn.FileContractID(0)),
			Signature:     hostSig,
			CoveredFields: types.CoveredFields{FileContractRevisions: []uint64{0}},
		},
	}
	if err := s.WriteResponse(hostSignaturesResp, 30*time.Second); err != nil {
		return fmt.Errorf("failed to write host signatures: %w", err)
	}
	return nil
}

// rpcRenewAndClearContract is an RPC that renews a contract and clears the
// existing contract
func (sh *SessionHandler) rpcRenewAndClearContract(s *session) error {
	/*	currentHeight := sh.cm.Tip().Index.Height
		if err := s.ContractRevisable(currentHeight); err != nil {
			return s.WriteError(fmt.Errorf("contract not revisable: %w", err))
		}

		var req rpcRenewAndClearContractRequest
		if err := s.ReadRequest(&req, 10*minMessageSize, time.Minute); err != nil {
			return fmt.Errorf("failed to read renew request: %w", err)
		}

		renewalTxnSet := req.Transactions
		if len(renewalTxnSet) == 0 || len(renewalTxnSet[len(renewalTxnSet)-1].FileContracts) != 1 || len(renewalTxnSet[len(renewalTxnSet)-1].FileContractRevisions) != 1 {
			return s.WriteError(ErrTxnMissingContract)
		}

		renterPub := req.RenterKey
		// get the host's public key, current block height, and settings
		existingContract := s.contract.Revision
		clearingRevision := renewalTxn.FileContractRevisions[0]
		hostPub := types.SiaPublicKey{
			Algorithm: types.SignatureEd25519,
			Key:       sh.privateKey.Public().(ed25519.PublicKey),
		}
		settings, err := sh.Settings()
		if err != nil {
			s.WriteError(ErrHostInternalError)
			return fmt.Errorf("failed to get host settings: %w", err)
		}
		// get the contract from the transaction set
		renewalTxn := &renewalTxnSet[len(renewalTxnSet)-1]

		// create an initial revision for the renewed contract
		initialRevision := initialRevision(renewalTxn, hostPub, renterPub)
		renewalSigHash := hashRevision(initialRevision)
		clearingSigHash := hashRevision(clearingRevision)
		renewalHostSig := ed25519.Sign(sh.privateKey, renewalSigHash[:])
		clearingHostSig := ed25519.Sign(sh.privateKey, clearingSigHash[:])

		// calculate the "base" storage cost to the renter and risked collateral for
		// the host for the data already in the contract. If the contract height did
		// not increase, base costs are zero since the storage is already paid for.
		baseRenterCost := settings.ContractPrice
		var baseCollateral types.Currency
		if initialRevision.NewWindowEnd > existingContract.NewWindowEnd {
			extension := uint64(initialRevision.NewWindowEnd - existingContract.NewWindowEnd)
			baseRenterCost = baseRenterCost.Add(settings.StoragePrice.Mul64(initialRevision.NewFileSize).Mul64(extension))
			baseCollateral = settings.Collateral.Mul64(initialRevision.NewFileSize).Mul64(extension)
		} else if initialRevision.NewValidProofOutputs[1].Value.Cmp(baseCollateral.Add(baseRenterCost)) < 0 {
			return s.WriteError(errors.New("renewal rejected: insufficient host payout for storage and collateral"))
		}

		// validate fields of the clearing revision and renewal. note: the v1
		// contract type does not contain the public keys or signatures.
		if err := validateClearingRevision(existingContract, clearingRevision); err != nil {
			return s.WriteError(fmt.Errorf("renewal rejected: clearing revision validation failed: %w", err))
		} else if err := validateContractRenewal(existingContract, initialRevision, hostPub, renterPub, baseRenterCost, baseCollateral, currentHeight, settings); err != nil {
			return s.WriteError(fmt.Errorf("renewal rejected: renewal validation failed: %w", err))
		}

		renterInputs, renterOutputs := len(renewalTxn.SiacoinInputs), len(renewalTxn.SiacoinOutputs)
		fundAmount := initialRevision.NewValidProofOutputs[1].Value.Sub(baseRenterCost)
		toSign, discard, err := sh.wallet.FundTransaction(renewalTxn, fundAmount, nil)
		if err != nil {
			s.WriteError(ErrHostInternalError)
			return fmt.Errorf("failed to fund renewal transaction: %w", err)
		}
		defer discard()

		// send the renter the host additions to the renewal txn
		hostAdditionsResp := &rpcFormContractAdditions{
			Inputs:  renewalTxn.SiacoinInputs[renterInputs:],
			Outputs: renewalTxn.SiacoinOutputs[renterOutputs:],
		}
		if err = s.WriteResponse(hostAdditionsResp, 30*time.Second); err != nil {
			return fmt.Errorf("failed to write host additions: %w", err)
		}

		// read the renter's signatures for the renewal
		var renterSigsResp rpcRenewAndClearContractSignatures
		if err = s.ReadResponse(&renterSigsResp, 4096, 30*time.Second); err != nil {
			return fmt.Errorf("failed to read renter signatures: %w", err)
		}

		// validate the renter's initial revision signature
		renterRenewalSig := renterSigsResp.
		if !ed25519.Verify(renterPub.Key, renewalSigHash[:], renterRenewalSig) {
			return s.WriteError(ErrInvalidRenterSignature)
		}
		renterClearingSig := renterSigsResp.FinalRevisionSignature
		// add the renter's signatures to the transaction and contract revision
		renewalTxn.TransactionSignatures = renterSigsResp.ContractSignatures

		// sign and broadcast the formation transaction
		if err = sh.wallet.SignTransaction(renewalTxn, toSign, types.FullCoveredFields); err != nil {
			s.WriteError(ErrHostInternalError)
			return fmt.Errorf("failed to sign formation transaction: %w", err)
		} else if err = sh.tpool.AcceptTransactionSet(renewalTxnSet); err != nil {
			return s.WriteError(fmt.Errorf("failed to broadcast formation transaction: %w", err))
		}

		clearingSignedRevision := contracts.SignedRevision{
			Revision:        existingContract,
			RenterSignature: renterSigsResp.FinalRevisionSignature,
			HostSignature:   clearingHostSig,
		}

		renewalSignedRevision := contracts.SignedRevision{
			Revision:        initialRevision,
			RenterSignature: renterRenewalSig,
			HostSignature:   hostRenewalSig,
		}
		if err := sh.contracts.RenewContract(renewalSignedRevision, clearingSignedRevision, renewalTxnSet, hostCollateral, currentHeight); err != nil {
			s.WriteError(ErrHostInternalError)
			return fmt.Errorf("failed to renew contract: %w", err)
		}

		// add renter spending to the amount spent
		s.Spend(baseRenterCost)
		// log the formation event
		sh.metrics.Report(EventContractRenewed{
			SessionUID: s.uid,
			ContractID: renewalTxn.FileContractID(0),
			Contract:   initialRevision,
		})

		// send the host signatures to the renter
		renterTxnSigs := len(renterSigsResp.ContractSignatures)
		hostSignaturesResp := &rpcFormContractSignatures{
			ContractSignatures: renewalTxn.TransactionSignatures[renterTxnSigs:],
			RevisionSignature: types.TransactionSignature{
				ParentID:      crypto.Hash(renewalTxn.FileContractID(0)),
				Signature:     hostSig,
				CoveredFields: types.CoveredFields{FileContractRevisions: []uint64{0}},
			},
		}
		if err := s.WriteResponse(hostSignaturesResp, 30*time.Second); err != nil {
			return fmt.Errorf("failed to write host signatures: %w", err)
		}*/
	return nil
}

// rpcSectorRoots returns the Merkle roots of the sectors in a contract
func (sh *SessionHandler) rpcSectorRoots(s *session) error {
	currentHeight := sh.cm.Tip().Index.Height
	if err := s.ContractRevisable(currentHeight); err != nil {
		return s.WriteError(fmt.Errorf("contract not revisable: %w", err))
	}

	var req rpcSectorRootsRequest
	if err := s.ReadRequest(&req, minMessageSize, 30*time.Second); err != nil {
		return fmt.Errorf("failed to read sector roots request: %w", err)
	}

	settings, err := sh.Settings()
	if err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to get host settings: %w", err)
	}

	revision, err := revise(s.contract.Revision, req.NewRevisionNumber, req.NewValidProofValues, req.NewMissedProofValues)
	if err != nil {
		return s.WriteError(fmt.Errorf("failed to revise contract: %w", err))
	}
	// validate the renter's signature
	sigHash := hashRevision(revision)
	if !ed25519.Verify(s.contract.RenterKey(), sigHash[:], req.Signature) {
		return s.WriteError(fmt.Errorf("failed to validate revision: %w", ErrInvalidRenterSignature))
	}
	hostSig := ed25519.Sign(sh.privateKey, sigHash[:])

	proofSize := merkle.RangeProofSize(merkle.LeavesPerSector, req.RootOffset, req.RootOffset+req.NumRoots)
	cost := settings.DownloadBandwidthPrice.Mul64((req.NumRoots + proofSize) * crypto.HashSize)
	if err := validateRevision(s.contract.Revision, revision, cost, types.ZeroCurrency); err != nil {
		return s.WriteError(fmt.Errorf("failed to validate revision: %w", err))
	}

	roots, err := sh.contracts.SectorRoots(s.contract.Revision.ParentID, req.NumRoots, req.RootOffset)
	if err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to get sector roots: %w", err)
	}

	// commit the revision
	signedRevision := contracts.SignedRevision{
		Revision:        revision,
		RenterSignature: req.Signature,
		HostSignature:   hostSig,
	}
	updater, err := sh.contracts.ReviseContract(revision.ParentID)
	if err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to revise contract: %w", err)
	} else if err := updater.Commit(signedRevision); err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to commit contract revision: %w", err)
	}
	s.contract = signedRevision

	sectorRootsResp := &rpcSectorRootsResponse{
		SectorRoots: roots,
		MerkleProof: merkle.BuildSectorRangeProof(roots, req.RootOffset, req.RootOffset+req.NumRoots),
		Signature:   hostSig,
	}
	if err := s.WriteResponse(sectorRootsResp, 2*time.Minute); err != nil {
		return fmt.Errorf("failed to write sector roots response: %w", err)
	}

	s.Spend(cost)
	return nil
}

func (sh *SessionHandler) rpcWrite(s *session) error {
	currentHeight := sh.cm.Tip().Index.Height
	// get the locked contract and check that it is revisable
	if err := s.ContractRevisable(currentHeight); err != nil {
		return s.WriteError(fmt.Errorf("contract not revisable: %w", err))
	}

	settings, err := sh.Settings()
	if err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to get settings: %w", err)
	}

	var req rpcWriteRequest
	if err := s.ReadRequest(&req, 5*SectorSize, 5*time.Minute); err != nil {
		return fmt.Errorf("failed to read write request: %w", err)
	}

	remainingDuration := uint64(s.contract.Revision.NewWindowStart) - currentHeight
	// validate the requested actions
	oldSectors := s.contract.Revision.NewFileSize / SectorSize
	cost, collateral, err := validateWriteActions(req.Actions, oldSectors, req.MerkleProof, remainingDuration, settings)
	if err != nil {
		return s.WriteError(fmt.Errorf("failed to validate write actions: %w", err))
	}

	// revise and validate the new revision
	revision, err := revise(s.contract.Revision, req.NewRevisionNumber, req.NewValidProofValues, req.NewMissedProofValues)
	if err != nil {
		return s.WriteError(fmt.Errorf("failed to revise contract: %w", err))
	} else if err := validateRevision(s.contract.Revision, revision, cost, collateral); err != nil {
		return s.WriteError(fmt.Errorf("failed to validate revision: %w", err))
	}
	contractUpdater, err := sh.contracts.ReviseContract(revision.ParentID)
	if err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to revise contract: %w", err)
	}
	oldRoots := contractUpdater.SectorRoots()
	for _, action := range req.Actions {
		switch action.Type {
		case rpcWriteActionAppend:
			root := merkle.SectorRoot(action.Data)

			release, err := sh.storage.WriteSector(storage.SectorRoot(root), action.Data)
			if err != nil {
				return s.WriteError(fmt.Errorf("append action: failed to write sector: %w", err))
			}
			defer release()
			contractUpdater.AppendSector(root)
		case rpcWriteActionTrim:
			if err := contractUpdater.TrimSectors(action.A); err != nil {
				return s.WriteError(fmt.Errorf("trim action: failed to trim sectors: %w", err))
			}
		case rpcWriteActionSwap:
			if err := contractUpdater.SwapSectors(action.A, action.B); err != nil {
				return s.WriteError(fmt.Errorf("swap action: failed to swap sectors: %w", err))
			}
		case rpcWriteActionUpdate:
			root, err := contractUpdater.SectorRoot(action.A)
			if err != nil {
				return s.WriteError(fmt.Errorf("update action: failed to get sector root: %w", err))
			}

			sector, err := sh.storage.ReadSector(storage.SectorRoot(root))
			if err != nil {
				s.WriteError(ErrHostInternalError)
				return fmt.Errorf("failed to read sector %v: %w", root, err)
			}

			i, offset := action.A, action.B
			if offset > SectorSize {
				return s.WriteError(fmt.Errorf("update action: invalid offset %v bytes", offset))
			} else if offset+uint64(len(action.Data)) > SectorSize {
				return s.WriteError(errors.New("update action: offset + data exceeds sector size"))
			}

			copy(sector[offset:], action.Data)
			newRoot := merkle.SectorRoot(sector)

			if err := contractUpdater.UpdateSectors(newRoot, i); err != nil {
				return s.WriteError(fmt.Errorf("update action: failed to update sector: %w", err))
			}
			release, err := sh.storage.WriteSector(storage.SectorRoot(root), action.Data)
			if err != nil {
				return s.WriteError(fmt.Errorf("append action: failed to write sector: %w", err))
			}
			defer release()
		}
	}

	// build the merkle proof response
	writeResp := &rpcWriteMerkleProof{
		NewMerkleRoot: contractUpdater.MerkleRoot(),
	}
	if req.MerkleProof {
		writeResp.OldSubtreeHashes, writeResp.OldLeafHashes = buildDiffProof(req.Actions, oldRoots)
	}
	if err := s.WriteResponse(writeResp, time.Minute); err != nil {
		return fmt.Errorf("failed to write merkle proof: %w", err)
	}

	// apply the new merkle root and file size to the revision
	revision.NewFileMerkleRoot = writeResp.NewMerkleRoot
	revision.NewFileSize = uint64(contractUpdater.SectorLength()) * SectorSize

	// read the renter's signature
	var renterSigResponse rpcWriteResponse
	if err := s.ReadResponse(&renterSigResponse, minMessageSize, 30*time.Second); err != nil {
		return fmt.Errorf("failed to read renter signature: %w", err)
	}
	// validate the contract signature
	renterSig := renterSigResponse.Signature
	sigHash := hashRevision(revision)
	if !ed25519.Verify(s.contract.RenterKey(), sigHash[:], renterSigResponse.Signature) {
		return s.WriteError(fmt.Errorf("failed to verify renter signature: %w", ErrInvalidRenterSignature))
	}
	hostSig := ed25519.Sign(sh.privateKey, sigHash[:])
	signedRevision := contracts.SignedRevision{
		Revision:        revision,
		HostSignature:   hostSig,
		RenterSignature: renterSig,
	}

	// sync the storage manager
	if err := sh.storage.Sync(); err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to sync storage manager: %w", err)
	}
	// commit the contract modifications
	if err := contractUpdater.Commit(signedRevision); err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to commit contract modifications: %w", err)
	}
	// update the session contract
	s.contract = signedRevision

	// add the amount spent
	s.Spend(cost)

	// send the host signature
	hostSigResp := &rpcWriteResponse{Signature: hostSig}
	if err := s.WriteResponse(hostSigResp, 30*time.Second); err != nil {
		return fmt.Errorf("failed to write host signature: %w", err)
	}
	return nil
}

func (sh *SessionHandler) rpcRead(s *session) error {
	currentHeight := sh.cm.Tip().Index.Height
	// get the locked contract and check that it is revisable
	if err := s.ContractRevisable(currentHeight); err != nil {
		return s.WriteError(fmt.Errorf("contract not revisable: %w", err))
	}

	// get the host's current settings
	settings, err := sh.Settings()
	if err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to get host settings: %w", err)
	}

	// read the read request
	var req rpcReadRequest
	if err := s.ReadRequest(&req, 4*minMessageSize, time.Minute); err != nil {
		return fmt.Errorf("failed to read read request: %w", err)
	}

	// validate the request sections and calculate the cost
	var bandwidth uint64
	for _, sec := range req.Sections {
		switch {
		case uint64(sec.Offset)+uint64(sec.Length) > SectorSize:
			return s.WriteError(errors.New("request is out-of-bounds"))
		case sec.Length == 0:
			return s.WriteError(errors.New("length cannot be zero"))
		case req.MerkleProof && (sec.Offset%merkle.LeafSize != 0 || sec.Length%merkle.LeafSize != 0):
			return s.WriteError(errors.New("offset and length must be multiples of SegmentSize when requesting a Merkle proof"))
		}

		bandwidth += uint64(sec.Length)
		if req.MerkleProof {
			start := sec.Offset / merkle.LeafSize
			end := (sec.Offset + sec.Length) / merkle.LeafSize
			proofSize := merkle.RangeProofSize(merkle.LeavesPerSector, start, end)
			bandwidth += proofSize * crypto.HashSize
		}
	}
	// revise the contract with the values sent by the renter
	revision, err := revise(s.contract.Revision, req.NewRevisionNumber, req.NewValidProofValues, req.NewMissedProofValues)
	if err != nil {
		return s.WriteError(fmt.Errorf("failed to revise contract: %w", err))
	}

	// calculate the cost of the read
	cost := settings.DownloadBandwidthPrice.Mul64(bandwidth).Add(settings.SectorAccessPrice.Mul64(uint64(len(req.Sections))))
	// validate the renter's signature and transfer
	sigHash := hashRevision(revision)
	if !ed25519.Verify(s.contract.RenterKey(), sigHash[:], req.Signature) {
		return s.WriteError(fmt.Errorf("failed to validate revision: %w", ErrInvalidRenterSignature))
	} else if err := validateRevision(s.contract.Revision, revision, cost, types.ZeroCurrency); err != nil {
		return s.WriteError(fmt.Errorf("failed to validate revision: %w", err))
	}

	// sign and commit the new revision
	hostSig := ed25519.Sign(sh.privateKey, sigHash[:])
	signedRevision := contracts.SignedRevision{
		Revision:        revision,
		HostSignature:   hostSig,
		RenterSignature: req.Signature,
	}

	updater, err := sh.contracts.ReviseContract(revision.ParentID)
	if err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to revise contract: %w", err)
	} else if err := updater.Commit(signedRevision); err != nil {
		s.WriteError(ErrHostInternalError)
		return fmt.Errorf("failed to commit contract revision: %w", err)
	}
	// update the session contract
	s.contract = signedRevision
	// add the cost to the amount spent
	s.Spend(cost)

	// listen for RPCLoopReadStop
	stopSignal := make(chan error, 1)
	go func() {
		var id Specifier
		// long timeout because the renter may be slow to send the stop signal
		err := s.ReadResponse(&id, minMessageSize, 5*time.Minute)
		if err != nil {
			stopSignal <- err
		} else if id != rpcReadStop {
			stopSignal <- errors.New("expected 'stop' from renter, got " + id.String())
		} else {
			stopSignal <- nil
		}
	}()

	// enter response loop
	for i, sec := range req.Sections {
		sector, err := sh.storage.ReadSector(storage.SectorRoot(sec.MerkleRoot))
		if err != nil {
			return s.WriteError(fmt.Errorf("failed to get sector: %w", err))
		}

		resp := &rpcReadResponse{
			Data: sector[sec.Offset : sec.Offset+sec.Length],
		}
		if req.MerkleProof {
			start := sec.Offset / merkle.LeafSize
			end := (sec.Offset + sec.Length) / merkle.LeafSize
			resp.MerkleProof = merkle.BuildProof(sector, start, end, nil)
		}

		// check for the stop signal and send the response
		select {
		case err := <-stopSignal:
			if err != nil {
				return err
			}
			resp.Signature = hostSig
			return s.WriteResponse(resp, 30*time.Second)
		default:
		}

		if i == len(req.Sections)-1 {
			resp.Signature = hostSig
		}
		if err := s.WriteResponse(resp, 30*time.Second); err != nil {
			return fmt.Errorf("failed to write read response: %w", err)
		}
	}
	return <-stopSignal
}
