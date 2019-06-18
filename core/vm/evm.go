// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package vm

import (
	"errors"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/DxChainNetwork/godx/common"
	"github.com/DxChainNetwork/godx/core/types"
	"github.com/DxChainNetwork/godx/crypto"
	"github.com/DxChainNetwork/godx/ethdb"
	"github.com/DxChainNetwork/godx/log"
	"github.com/DxChainNetwork/godx/params"
	"github.com/DxChainNetwork/godx/rlp"
)

// emptyCodeHash is used by create to ensure deployment is disallowed to already
// deployed contract addresses (relevant after the account abstraction).
var (
	emptyCodeHash = crypto.Keccak256Hash(nil)

	errUnknownStorageContractTx = errors.New("unknown storage contract tx")
)

type (
	// CanTransferFunc is the signature of a transfer guard function
	CanTransferFunc func(StateDB, common.Address, *big.Int) bool
	// TransferFunc is the signature of a transfer function
	TransferFunc func(StateDB, common.Address, common.Address, *big.Int)
	// GetHashFunc returns the nth block hash in the blockchain
	// and is used by the BLOCKHASH EVM op code.
	GetHashFunc func(uint64) common.Hash
)

// run runs the given contract and takes care of running precompiles with a fallback to the byte code interpreter.
func run(evm *EVM, contract *Contract, input []byte, readOnly bool) ([]byte, error) {
	if contract.CodeAddr != nil {
		precompiles := PrecompiledContractsHomestead
		if evm.ChainConfig().IsByzantium(evm.BlockNumber) {
			precompiles = PrecompiledContractsByzantium
		}
		if p := precompiles[*contract.CodeAddr]; p != nil {
			return RunPrecompiledContract(p, input, contract)
		}
	}
	for _, interpreter := range evm.interpreters {
		if interpreter.CanRun(contract.Code) {
			if evm.interpreter != interpreter {
				// Ensure that the interpreter pointer is set back
				// to its current value upon return.
				defer func(i Interpreter) {
					evm.interpreter = i
				}(evm.interpreter)
				evm.interpreter = interpreter
			}
			return interpreter.Run(contract, input, readOnly)
		}
	}
	return nil, ErrNoCompatibleInterpreter
}

// Context provides the EVM with auxiliary information. Once provided
// it shouldn't be modified.
type Context struct {
	// CanTransfer returns whether the account contains
	// sufficient ether to transfer the value
	CanTransfer CanTransferFunc
	// Transfer transfers ether from one account to the other
	Transfer TransferFunc
	// GetHash returns the hash corresponding to n
	GetHash GetHashFunc

	// Message information
	Origin   common.Address // Provides information for ORIGIN
	GasPrice *big.Int       // Provides information for GASPRICE

	// Block information
	Coinbase    common.Address // Provides information for COINBASE
	GasLimit    uint64         // Provides information for GASLIMIT
	BlockNumber *big.Int       // Provides information for NUMBER
	Time        *big.Int       // Provides information for TIME
	Difficulty  *big.Int       // Provides information for DIFFICULTY
}

// EVM is the Ethereum Virtual Machine base object and provides
// the necessary tools to run a contract on the given state with
// the provided context. It should be noted that any error
// generated through any of the calls should be considered a
// revert-state-and-consume-all-gas operation, no checks on
// specific errors should ever be performed. The interpreter makes
// sure that any errors generated are to be considered faulty code.
//
// The EVM should never be reused and is not thread safe.
type EVM struct {
	// Context provides auxiliary blockchain related information
	Context
	// StateDB gives access to the underlying state
	StateDB StateDB
	// Depth is the current call stack
	depth int

	// chainConfig contains information about the current chain
	chainConfig *params.ChainConfig
	// chain rules contains the chain rules for the current epoch
	chainRules params.Rules
	// virtual machine configuration options used to initialise the
	// evm.
	vmConfig Config
	// global (to this context) ethereum virtual machine
	// used throughout the execution of the tx.
	interpreters []Interpreter
	interpreter  Interpreter
	// abort is used to abort the EVM calling operations
	// NOTE: must be set atomically
	abort int32
	// callGasTemp holds the gas available for the current call. This is needed because the
	// available gas is calculated in gasCall* according to the 63/64 rule and later
	// applied in opCall*.
	callGasTemp uint64
}

// NewEVM returns a new EVM. The returned EVM is not thread safe and should
// only ever be used *once*.
func NewEVM(ctx Context, statedb StateDB, chainConfig *params.ChainConfig, vmConfig Config) *EVM {
	evm := &EVM{
		Context:      ctx,
		StateDB:      statedb,
		vmConfig:     vmConfig,
		chainConfig:  chainConfig,
		chainRules:   chainConfig.Rules(ctx.BlockNumber),
		interpreters: make([]Interpreter, 0, 1),
	}

	if chainConfig.IsEWASM(ctx.BlockNumber) {
		// to be implemented by EVM-C and Wagon PRs.
		// if vmConfig.EWASMInterpreter != "" {
		//  extIntOpts := strings.Split(vmConfig.EWASMInterpreter, ":")
		//  path := extIntOpts[0]
		//  options := []string{}
		//  if len(extIntOpts) > 1 {
		//    options = extIntOpts[1..]
		//  }
		//  evm.interpreters = append(evm.interpreters, NewEVMVCInterpreter(evm, vmConfig, options))
		// } else {
		// 	evm.interpreters = append(evm.interpreters, NewEWASMInterpreter(evm, vmConfig))
		// }
		panic("No supported ewasm interpreter yet.")
	}

	// vmConfig.EVMInterpreter will be used by EVM-C, it won't be checked here
	// as we always want to have the built-in EVM as the failover option.
	evm.interpreters = append(evm.interpreters, NewEVMInterpreter(evm, vmConfig))
	evm.interpreter = evm.interpreters[0]

	return evm
}

// Cancel cancels any running EVM operation. This may be called concurrently and
// it's safe to be called multiple times.
func (evm *EVM) Cancel() {
	atomic.StoreInt32(&evm.abort, 1)
}

// Interpreter returns the current interpreter
func (evm *EVM) Interpreter() Interpreter {
	return evm.interpreter
}

// Call executes the contract associated with the addr with the given input as
// parameters. It also handles any necessary value transfer required and takes
// the necessary steps to create accounts and reverses the state in case of an
// execution error or failed value transfer.
func (evm *EVM) Call(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int) (ret []byte, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, gas, nil
	}

	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	// Fail if we're trying to transfer more than the available balance
	if !evm.Context.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, gas, ErrInsufficientBalance
	}

	var (
		to       = AccountRef(addr)
		snapshot = evm.StateDB.Snapshot()
	)
	if !evm.StateDB.Exist(addr) {
		precompiles := PrecompiledContractsHomestead
		if evm.ChainConfig().IsByzantium(evm.BlockNumber) {
			precompiles = PrecompiledContractsByzantium
		}
		if precompiles[addr] == nil && evm.ChainConfig().IsEIP158(evm.BlockNumber) && value.Sign() == 0 {
			// Calling a non existing account, don't do anything, but ping the tracer
			if evm.vmConfig.Debug && evm.depth == 0 {
				evm.vmConfig.Tracer.CaptureStart(caller.Address(), addr, false, input, gas, value)
				evm.vmConfig.Tracer.CaptureEnd(ret, 0, 0, nil)
			}
			return nil, gas, nil
		}
		evm.StateDB.CreateAccount(addr)
	}
	evm.Transfer(evm.StateDB, caller.Address(), to.Address(), value)
	// Initialise a new contract and set the code that is to be used by the EVM.
	// The contract is a scoped environment for this execution context only.
	contract := NewContract(caller, to, value, gas)
	contract.SetCallCode(&addr, evm.StateDB.GetCodeHash(addr), evm.StateDB.GetCode(addr))

	// Even if the account has no code, we need to continue because it might be a precompile
	start := time.Now()

	// Capture the tracer start/end events in debug mode
	if evm.vmConfig.Debug && evm.depth == 0 {
		evm.vmConfig.Tracer.CaptureStart(caller.Address(), addr, false, input, gas, value)

		defer func() { // Lazy evaluation of the parameters
			evm.vmConfig.Tracer.CaptureEnd(ret, gas-contract.Gas, time.Since(start), err)
		}()
	}
	ret, err = run(evm, contract, input, false)

	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in homestead this also counts for code storage gas errors.
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != errExecutionReverted {
			contract.UseGas(contract.Gas)
		}
	}
	return ret, contract.Gas, err
}

// CallCode executes the contract associated with the addr with the given input
// as parameters. It also handles any necessary value transfer required and takes
// the necessary steps to create accounts and reverses the state in case of an
// execution error or failed value transfer.
//
// CallCode differs from Call in the sense that it executes the given address'
// code with the caller as context.
func (evm *EVM) CallCode(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int) (ret []byte, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, gas, nil
	}

	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	// Fail if we're trying to transfer more than the available balance
	if !evm.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, gas, ErrInsufficientBalance
	}

	var (
		snapshot = evm.StateDB.Snapshot()
		to       = AccountRef(caller.Address())
	)
	// initialise a new contract and set the code that is to be used by the
	// EVM. The contract is a scoped environment for this execution context
	// only.
	contract := NewContract(caller, to, value, gas)
	contract.SetCallCode(&addr, evm.StateDB.GetCodeHash(addr), evm.StateDB.GetCode(addr))

	ret, err = run(evm, contract, input, false)
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != errExecutionReverted {
			contract.UseGas(contract.Gas)
		}
	}
	return ret, contract.Gas, err
}

// DelegateCall executes the contract associated with the addr with the given input
// as parameters. It reverses the state in case of an execution error.
//
// DelegateCall differs from CallCode in the sense that it executes the given address'
// code with the caller as context and the caller is set to the caller of the caller.
func (evm *EVM) DelegateCall(caller ContractRef, addr common.Address, input []byte, gas uint64) (ret []byte, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, gas, nil
	}
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}

	var (
		snapshot = evm.StateDB.Snapshot()
		to       = AccountRef(caller.Address())
	)

	// Initialise a new contract and make initialise the delegate values
	contract := NewContract(caller, to, nil, gas).AsDelegate()
	contract.SetCallCode(&addr, evm.StateDB.GetCodeHash(addr), evm.StateDB.GetCode(addr))

	ret, err = run(evm, contract, input, false)
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != errExecutionReverted {
			contract.UseGas(contract.Gas)
		}
	}
	return ret, contract.Gas, err
}

// StaticCall executes the contract associated with the addr with the given input
// as parameters while disallowing any modifications to the state during the call.
// Opcodes that attempt to perform such modifications will result in exceptions
// instead of performing the modifications.
func (evm *EVM) StaticCall(caller ContractRef, addr common.Address, input []byte, gas uint64) (ret []byte, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, gas, nil
	}
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}

	var (
		to       = AccountRef(addr)
		snapshot = evm.StateDB.Snapshot()
	)
	// Initialise a new contract and set the code that is to be used by the
	// EVM. The contract is a scoped environment for this execution context
	// only.
	contract := NewContract(caller, to, new(big.Int), gas)
	contract.SetCallCode(&addr, evm.StateDB.GetCodeHash(addr), evm.StateDB.GetCode(addr))

	// We do an AddBalance of zero here, just in order to trigger a touch.
	// This doesn't matter on Mainnet, where all empties are gone at the time of Byzantium,
	// but is the correct thing to do and matters on other networks, in tests, and potential
	// future scenarios
	evm.StateDB.AddBalance(addr, bigZero)

	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in Homestead this also counts for code storage gas errors.
	ret, err = run(evm, contract, input, true)
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != errExecutionReverted {
			contract.UseGas(contract.Gas)
		}
	}
	return ret, contract.Gas, err
}

type codeAndHash struct {
	code []byte
	hash common.Hash
}

func (c *codeAndHash) Hash() common.Hash {
	if c.hash == (common.Hash{}) {
		c.hash = crypto.Keccak256Hash(c.code)
	}
	return c.hash
}

// create creates a new contract using code as deployment code.
func (evm *EVM) create(caller ContractRef, codeAndHash *codeAndHash, gas uint64, value *big.Int, address common.Address) ([]byte, common.Address, uint64, error) {
	// Depth check execution. Fail if we're trying to execute above the
	// limit.
	if evm.depth > int(params.CallCreateDepth) {
		return nil, common.Address{}, gas, ErrDepth
	}
	if !evm.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, common.Address{}, gas, ErrInsufficientBalance
	}
	nonce := evm.StateDB.GetNonce(caller.Address())
	evm.StateDB.SetNonce(caller.Address(), nonce+1)

	// Ensure there's no existing contract already at the designated address
	contractHash := evm.StateDB.GetCodeHash(address)
	if evm.StateDB.GetNonce(address) != 0 || (contractHash != (common.Hash{}) && contractHash != emptyCodeHash) {
		return nil, common.Address{}, 0, ErrContractAddressCollision
	}
	// Create a new account on the state
	snapshot := evm.StateDB.Snapshot()
	evm.StateDB.CreateAccount(address)
	if evm.ChainConfig().IsEIP158(evm.BlockNumber) {
		evm.StateDB.SetNonce(address, 1)
	}
	evm.Transfer(evm.StateDB, caller.Address(), address, value)

	// initialise a new contract and set the code that is to be used by the
	// EVM. The contract is a scoped environment for this execution context
	// only.
	contract := NewContract(caller, AccountRef(address), value, gas)
	contract.SetCodeOptionalHash(&address, codeAndHash)

	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, address, gas, nil
	}

	if evm.vmConfig.Debug && evm.depth == 0 {
		evm.vmConfig.Tracer.CaptureStart(caller.Address(), address, true, codeAndHash.code, gas, value)
	}
	start := time.Now()

	ret, err := run(evm, contract, nil, false)

	// check whether the max code size has been exceeded
	maxCodeSizeExceeded := evm.ChainConfig().IsEIP158(evm.BlockNumber) && len(ret) > params.MaxCodeSize
	// if the contract creation ran successfully and no errors were returned
	// calculate the gas required to store the code. If the code could not
	// be stored due to not enough gas set an error and let it be handled
	// by the error checking condition below.
	if err == nil && !maxCodeSizeExceeded {
		createDataGas := uint64(len(ret)) * params.CreateDataGas
		if contract.UseGas(createDataGas) {
			evm.StateDB.SetCode(address, ret)
		} else {
			err = ErrCodeStoreOutOfGas
		}
	}

	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in homestead this also counts for code storage gas errors.
	if maxCodeSizeExceeded || (err != nil && (evm.ChainConfig().IsHomestead(evm.BlockNumber) || err != ErrCodeStoreOutOfGas)) {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != errExecutionReverted {
			contract.UseGas(contract.Gas)
		}
	}
	// Assign err if contract code size exceeds the max while the err is still empty.
	if maxCodeSizeExceeded && err == nil {
		err = errMaxCodeSizeExceeded
	}
	if evm.vmConfig.Debug && evm.depth == 0 {
		evm.vmConfig.Tracer.CaptureEnd(ret, gas-contract.Gas, time.Since(start), err)
	}
	return ret, address, contract.Gas, err

}

// Create creates a new contract using code as deployment code.
func (evm *EVM) Create(caller ContractRef, code []byte, gas uint64, value *big.Int) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error) {
	contractAddr = crypto.CreateAddress(caller.Address(), evm.StateDB.GetNonce(caller.Address()))
	return evm.create(caller, &codeAndHash{code: code}, gas, value, contractAddr)
}

// Create2 creates a new contract using code as deployment code.
//
// The different between Create2 with Create is Create2 uses sha3(0xff ++ msg.sender ++ salt ++ sha3(init_code))[12:]
// instead of the usual sender-and-nonce-hash as the address where the contract is initialized at.
func (evm *EVM) Create2(caller ContractRef, code []byte, gas uint64, endowment *big.Int, salt *big.Int) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error) {
	codeAndHash := &codeAndHash{code: code}
	contractAddr = crypto.CreateAddress2(caller.Address(), common.BigToHash(salt), codeAndHash.Hash().Bytes())
	return evm.create(caller, codeAndHash, gas, endowment, contractAddr)
}

// ChainConfig returns the environment's chain configuration
func (evm *EVM) ChainConfig() *params.ChainConfig { return evm.chainConfig }

// ApplyStorageContractTransaction distinguish and execute transactions
func (evm *EVM) ApplyStorageContractTransaction(caller ContractRef, txType string, data []byte, gas uint64) (ret []byte, leftOverGas uint64, err error) {

	switch txType {
	case HostAnnounceTransaction:
		return evm.HostAnnounceTx(caller, data, gas)
	case ContractCreateTransaction:
		return evm.ContractCreateTx(caller, data, gas)
	case CommitRevisionTransaction:
		return evm.CommitRevisionTx(caller, data, gas)
	case StorageProofTransaction:
		return evm.StorageProofTx(caller, data, gas)
	default:
		return nil, gas, errUnknownStorageContractTx
	}

}

// HostAnnounceTx host declares its own information on the chain
func (evm *EVM) HostAnnounceTx(caller ContractRef, data []byte, gas uint64) ([]byte, uint64, error) {
	log.Info("enter host announce tx executing ... ")
	var (
		snapshot = evm.StateDB.Snapshot()
		err      error
	)

	scSet := types.StorageContractSet{}
	gasDecode, resultDecode := RemainGas(gas, rlp.DecodeBytes, data, &scSet)
	errDec, _ := resultDecode[0].(error)
	if errDec != nil {
		return nil, gasDecode, errDec
	}

	HostInfo := scSet.HostAnnounce
	gasCheck, resultCheck := RemainGas(gasDecode, CheckMultiSignatures, HostInfo, uint64(0), [][]byte{HostInfo.Signature})
	errCheck, _ := resultCheck[0].(error)
	if errCheck != nil {
		log.Error("failed to check signature for host announce", "err", errCheck)
		return nil, gasCheck, errCheck
	}

	// go back state DB if something is wrong above
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		return nil, gasCheck, err
	}

	log.Info("host announce tx execution done", "remain_gas", gas, "host_address", HostInfo.NetAddress)

	// return remain gas if everything is ok
	return nil, gasCheck, nil
}

// ContractCreateTx client sends a contract transaction with host
func (evm *EVM) ContractCreateTx(caller ContractRef, data []byte, gas uint64) ([]byte, uint64, error) {
	log.Info("enter form contract tx executing ... ")
	var (
		snapshot = evm.StateDB.Snapshot()
		err      error
		db       = evm.StateDB.Database().TrieDB().DiskDB().(ethdb.Database)
	)

	defer func() {
		if errInfo := recover(); errInfo != nil {
			err = errInfo.(error)
			log.Error("something wrong when executing form contract tx", "err", errInfo)
		}
	}()

	// rlp decode and calculate gas used
	scSet := types.StorageContractSet{}
	gasRemainDecode, resultDecode := RemainGas(gas, rlp.DecodeBytes, data, &scSet)
	errDecode, _ := resultDecode[0].(error)
	if errDecode != nil {
		return nil, gasRemainDecode, errDecode
	}

	storageContract := scSet.StorageContract

	// check form contract and calculate gas used
	currentHeight := evm.BlockNumber.Uint64()
	gasRemainCheck, resultCheck := RemainGas(gasRemainDecode, CheckFormContract, evm, storageContract, uint64(currentHeight))
	errCheck, _ := resultCheck[0].(error)
	if errCheck != nil {
		log.Error("failed to check form contract", "err", errCheck)
		return nil, gasRemainCheck, errCheck
	}

	// store file contract info to local DB and calculate gas used
	scID := storageContract.ID()
	gasRemainStore, resultStore := RemainGas(gasRemainCheck, StoreStorageContract, db, scID, storageContract)
	errStore, _ := resultStore[0].(error)
	if errStore != nil {
		return nil, gasRemainStore, errStore
	}

	// store file contract ID to local DB and calculate gas used
	gasRemainStoreExpire, resultStoreExpire := RemainGas(gasRemainStore, StoreExpireStorageContract, db, scID, storageContract.WindowEnd)
	errStoreExpire, _ := resultStoreExpire[0].(error)
	if errStoreExpire != nil {
		return nil, gasRemainStoreExpire, errStoreExpire
	}

	// deduct the collateral and deposit it to the public account
	clientAddr := storageContract.ClientCollateral.Address
	hostAddr := storageContract.HostCollateral.Address
	clientCollateralAmount := storageContract.ClientCollateral.Value
	hostCollateralAmount := storageContract.HostCollateral.Value
	evm.StateDB.SubBalance(clientAddr, clientCollateralAmount)
	evm.StateDB.SubBalance(hostAddr, hostCollateralAmount)

	// go back state DB and delete file contract from local DB if something is wrong above
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		errDel := DeleteStorageContract(db, scID)
		if errDel != nil {
			log.Error("failed to delete file contract from db", "error", errDel, "file_contract_id", common.Hash(scID).Hex())
		}
		errDelExp := DeleteExpireStorageContract(db, scID, storageContract.WindowEnd)
		if errDelExp != nil {
			log.Error("failed to delete expire file contract from db", "error", errDelExp, "file_contract_id", common.Hash(scID).Hex())
		}
		return nil, gasRemainStoreExpire, err
	}

	log.Info("form contract tx execution done", "remain_gas", gasRemainStoreExpire, "file_contract_id", common.Hash(scID).Hex())

	// return remain gas if everything is ok
	return nil, gasRemainStoreExpire, nil
}

// CommitRevisionTx host sends a revision transaction
func (evm *EVM) CommitRevisionTx(caller ContractRef, data []byte, gas uint64) ([]byte, uint64, error) {
	log.Info("enter file contract reversion tx executing ... ")
	var (
		snapshot = evm.StateDB.Snapshot()
		err      error
	)

	scSet := types.StorageContractSet{}
	gasRemainDecode, resultDecode := RemainGas(gas, rlp.DecodeBytes, data, &scSet)
	errDec, _ := resultDecode[0].(error)
	if errDec != nil {
		return nil, gasRemainDecode, errDec
	}

	storageContractRevision := scSet.StorageContractRevision

	// check file contract reversion and calculate gas used
	currentHeight := evm.BlockNumber.Uint64()
	gasRemainCheck, resultCheck := RemainGas(gasRemainDecode, CheckReversionContract, evm, storageContractRevision, uint64(currentHeight))
	errCheck, _ := resultCheck[0].(error)
	if errCheck != nil {
		log.Error("failed to check file contract reversion", "err", errCheck)
		return nil, gasRemainCheck, errCheck
	}

	db := evm.StateDB.Database().TrieDB().DiskDB().(ethdb.Database)
	scID := storageContractRevision.ParentID
	oldStorageContract, errGet := GetStorageContract(db, scID)
	if errGet != nil {
		return nil, gasRemainCheck, errGet
	}

	newStorageContract := types.StorageContract{
		FileSize:           storageContractRevision.NewFileSize,
		FileMerkleRoot:     storageContractRevision.NewFileMerkleRoot,
		WindowStart:        storageContractRevision.NewWindowStart,
		WindowEnd:          storageContractRevision.NewWindowEnd,
		ClientCollateral:   oldStorageContract.ClientCollateral,
		HostCollateral:     oldStorageContract.HostCollateral,
		ValidProofOutputs:  storageContractRevision.NewValidProofOutputs,
		MissedProofOutputs: storageContractRevision.NewMissedProofOutputs,
		UnlockHash:         storageContractRevision.NewUnlockHash,
		RevisionNumber:     storageContractRevision.NewRevisionNumber,
	}

	DeleteStorageContract(db, scID)
	DeleteExpireStorageContract(db, scID, oldStorageContract.WindowEnd)

	gasRemainStore, resultStore := RemainGas(gasRemainCheck, StoreStorageContract, db, scID, newStorageContract)
	errStore, _ := resultStore[0].(error)
	if errStore != nil {
		return nil, gasRemainStore, errStore
	}

	gasRemainStoreExpire, resultStoreExpire := RemainGas(gasRemainStore, StoreExpireStorageContract, db, scID, newStorageContract.WindowEnd)
	errStoreExpire, _ := resultStoreExpire[0].(error)
	if errStore != nil {
		return nil, gasRemainStoreExpire, errStoreExpire
	}

	// go back state DB if something is wrong above
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		errDel := DeleteStorageContract(db, scID)
		if errDel != nil {
			log.Error("failed to delete file contract from db", "error", errDel, "file_contract_id", common.Hash(scID).Hex())
		}
		errDelExp := DeleteExpireStorageContract(db, scID, newStorageContract.WindowEnd)
		if errDelExp != nil {
			log.Error("failed to delete expire file contract from db", "error", errDelExp, "file_contract_id", common.Hash(scID).Hex())
		}
		return nil, gasRemainStoreExpire, err
	}

	log.Info("file contract reversion tx execution done", "remain_gas", gasRemainStoreExpire, "file_contract_id", common.Hash(scID).Hex())

	return nil, gasRemainStoreExpire, nil
}

// StorageProofTx host send storage certificate transaction
func (evm *EVM) StorageProofTx(caller ContractRef, data []byte, gas uint64) ([]byte, uint64, error) {
	log.Info("enter storage proof tx executing ... ")
	var (
		snapshot = evm.StateDB.Snapshot()
		err      error
	)

	scSet := types.StorageContractSet{}
	gasRemainDec, resultDec := RemainGas(gas, rlp.DecodeBytes, data, &scSet)
	errDec, _ := resultDec[0].(error)
	if errDec != nil {
		return nil, gasRemainDec, errDec
	}

	sp := scSet.StorageProof
	currentHeight := evm.BlockNumber.Uint64()
	gasRemainCheck, resultCheck := RemainGas(gasRemainDec, CheckStorageProof, evm, sp, uint64(currentHeight))
	errCheck, _ := resultCheck[0].(error)
	if errCheck != nil {
		return nil, gasRemainCheck, errCheck
	}

	db := evm.StateDB.Database().TrieDB().DiskDB().(ethdb.Database)
	sc, errGet := GetStorageContract(db, sp.ParentID)
	if errGet != nil {
		return nil, gasRemainCheck, errGet
	}

	// effect valid proof outputs, first for client, second for host
	for _, vpo := range sc.ValidProofOutputs {
		evm.StateDB.AddBalance(vpo.Address, vpo.Value)
	}

	errDel := DeleteStorageContract(db, sp.ParentID)
	if errDel != nil {
		log.Error("failed to delete file contract for storage proof", "error", errDel)
	}
	errDelExp := DeleteExpireStorageContract(db, sp.ParentID, uint64(currentHeight))
	if errDelExp != nil {
		log.Error("failed to delete expire file contract for storage proof", "error", errDelExp)
	}

	// TODO: 全局的 err 根本没用到，其他的合约交易处理一样，后续需要调整下 。。
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		return nil, gasRemainCheck, err
	}

	log.Info("storage proof tx execution done", "file_contract_id", common.Hash(sp.ParentID).Hex())

	return nil, gasRemainCheck, nil
}
