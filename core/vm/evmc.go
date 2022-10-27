// Copyright 2019 The go-ethereum Authors
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

// Implements interaction with EVMC-based VMs.
// https://github.com/ethereum/evmc

package vm

import (
	"bytes"
	"fmt"
	"math/big"
	"unsafe"

	"strings"

	"github.com/ethereum/evmc/v10/bindings/go/evmc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// EVMC represents the reference to a common EVMC-based VM instance and
// the current execution context as required by go-ethereum design.
type EVMC struct {
	instance *evmc.VM        // The reference to the EVMC VM instance.
	env      *EVM            // The execution context.
	cap      evmc.Capability // The supported EVMC capability (EVM or Ewasm)
	readOnly bool            // The readOnly flag (TODO: Try to get rid of it).
}

var (
	evmModule   *evmc.VM
	ewasmModule *evmc.VM
)

func InitEVMCEVM(config string) {
	evmModule = initEVMC(evmc.CapabilityEVM1, config)
}

func InitEVMCEwasm(config string) {
	ewasmModule = initEVMC(evmc.CapabilityEWASM, config)
}

func initEVMC(cap evmc.Capability, config string) *evmc.VM {
	options := strings.Split(config, ",")
	path := options[0]

	if path == "" {
		panic("EVMC VM path not provided, set --vm.(evm|ewasm)=/path/to/vm")
	}

	instance, err := evmc.Load(path)
	if err != nil {
		panic(err.Error())
	}
	log.Info("EVMC VM loaded", "name", instance.Name(), "version", instance.Version(), "path", path)

	// Set options before checking capabilities.
	for _, option := range options[1:] {
		if idx := strings.Index(option, "="); idx >= 0 {
			name := option[:idx]
			value := option[idx+1:]
			err := instance.SetOption(name, value)
			if err == nil {
				log.Info("EVMC VM option set", "name", name, "value", value)
			} else {
				log.Warn("EVMC VM option setting failed", "name", name, "error", err)
			}
		}
	}

	if !instance.HasCapability(cap) {
		panic(fmt.Errorf("The EVMC module %s does not have requested capability %d", path, cap))
	}
	return instance
}

// hostContext implements evmc.HostContext interface.
type hostContext struct {
	env      *EVM      // The reference to the EVM execution context.
	contract *Contract // The reference to the current contract, needed by Call-like methods.
}

func (host *hostContext) AccountExists(addr evmc.Address) bool {
	if host.env.ChainConfig().IsEIP158(host.env.Context.BlockNumber) {
		if !host.env.StateDB.Empty(common.Address(addr)) {
			return true
		}
	} else if host.env.StateDB.Exist(common.Address(addr)) {
		return true
	}
	return false
}

func (host *hostContext) GetStorage(addr evmc.Address, key evmc.Hash) evmc.Hash {
	return evmc.Hash(host.env.StateDB.GetState(common.Address(addr), common.Hash(key)))
}

func (host *hostContext) SetStorage(addr evmc.Address, key evmc.Hash, value evmc.Hash) (status evmc.StorageStatus) {
	oldValue := host.env.StateDB.GetState(common.Address(addr), common.Hash(key))
	if evmc.Hash(oldValue) == value {
		return evmc.StorageAssigned
	}

	current := host.env.StateDB.GetState(common.Address(addr), common.Hash(key))
	original := host.env.StateDB.GetCommittedState(common.Address(addr), common.Hash(key))

	host.env.StateDB.SetState(common.Address(addr), common.Hash(key), common.Hash(value))

	hasNetStorageCostEIP := host.env.ChainConfig().IsConstantinople(host.env.Context.BlockNumber) &&
		!host.env.ChainConfig().IsPetersburg(host.env.Context.BlockNumber)
	if !hasNetStorageCostEIP {

		zero := common.Hash{}
		status = evmc.StorageModified
		if oldValue == zero {
			return evmc.StorageAdded
		} else if common.Hash(value) == zero {
			host.env.StateDB.AddRefund(params.SstoreRefundGas)
			return evmc.StorageDeleted
		}
		return evmc.StorageModified
	}

	if original == current {
		if original == (common.Hash{}) { // create slot (2.1.1)
			return evmc.StorageAdded
		}
		if common.Hash(value) == (common.Hash{}) { // delete slot (2.1.2b)
			host.env.StateDB.AddRefund(params.NetSstoreClearRefund)
			return evmc.StorageDeleted
		}
		return evmc.StorageModified
	}
	if original != (common.Hash{}) {
		if current == (common.Hash{}) { // recreate slot (2.2.1.1)
			host.env.StateDB.SubRefund(params.NetSstoreClearRefund)
		} else if common.Hash(value) == (common.Hash{}) { // delete slot (2.2.1.2)
			host.env.StateDB.AddRefund(params.NetSstoreClearRefund)
		}
	}
	if original == common.Hash(value) {
		if original == (common.Hash{}) { // reset to original inexistent slot (2.2.2.1)
			host.env.StateDB.AddRefund(params.NetSstoreResetClearRefund)
		} else { // reset to original existing slot (2.2.2.2)
			host.env.StateDB.AddRefund(params.NetSstoreResetRefund)
		}
	}
	return evmc.StorageModified
}

func (host *hostContext) GetBalance(addr evmc.Address) evmc.Hash {
	return evmc.Hash(common.BigToHash(host.env.StateDB.GetBalance(common.Address(addr))))
}

func (host *hostContext) GetCodeSize(addr evmc.Address) int {
	return host.env.StateDB.GetCodeSize(common.Address(addr))
}

func (host *hostContext) GetCodeHash(addr evmc.Address) evmc.Hash {
	if host.env.StateDB.Empty(common.Address(addr)) {
		return evmc.Hash{}
	}
	return evmc.Hash(host.env.StateDB.GetCodeHash(common.Address(addr)))
}

func (host *hostContext) GetCode(addr evmc.Address) []byte {
	return host.env.StateDB.GetCode(common.Address(addr))
}

func (host *hostContext) Selfdestruct(addr evmc.Address, beneficiary evmc.Address) bool {
	db := host.env.StateDB
	if !db.HasSuicided(common.Address(addr)) {
		db.AddRefund(params.SelfdestructRefundGas)
		return true
	}
	db.AddBalance(common.Address(beneficiary), db.GetBalance(common.Address(addr)))
	db.Suicide(common.Address(addr))
	return false
}

func (host *hostContext) GetTxContext() evmc.TxContext {
	return evmc.TxContext{
		GasPrice:  evmc.Hash(common.BigToHash(host.env.GasPrice)),
		Origin:    evmc.Address(host.env.Origin),
		Coinbase:  evmc.Address(host.env.Context.Coinbase),
		Number:    host.env.Context.BlockNumber.Int64(),
		Timestamp: host.env.Context.Time.Int64(),
		GasLimit:  int64(host.env.Context.GasLimit)}
}

func (host *hostContext) GetBlockHash(number int64) evmc.Hash {
	b := host.env.Context.BlockNumber.Int64()
	if number >= (b-256) && number < b {
		return evmc.Hash(host.env.Context.GetHash(uint64(number)))
	}
	return evmc.Hash{}
}

func (host *hostContext) EmitLog(addr evmc.Address, topics []evmc.Hash, data []byte) {
	topicsCommon := (*[]common.Hash)(unsafe.Pointer(&topics))
	host.env.StateDB.AddLog(&types.Log{
		Address:     common.Address(addr),
		Topics:      *topicsCommon,
		Data:        data,
		BlockNumber: host.env.Context.BlockNumber.Uint64(),
	})
}

func (host *hostContext) Call(kind evmc.CallKind,
	destination evmc.Address, sender evmc.Address, value evmc.Hash, input []byte, gas int64, depth int,
	static bool, salt evmc.Hash, codeAddress evmc.Address) (output []byte, gasLeft int64, gasRefund int64, createAddr evmc.Address, err error) {

	gasU := uint64(gas)
	var gasLeftU uint64

	switch kind {
	case evmc.Call:
		if static {
			output, gasLeftU, err = host.env.StaticCall(host.contract, common.Address(destination), input, gasU)
		} else {
			output, gasLeftU, err = host.env.Call(host.contract, common.Address(destination), input, gasU, new(big.Int).SetBytes(value[:]))
		}
	case evmc.DelegateCall:
		output, gasLeftU, err = host.env.DelegateCall(host.contract, common.Address(destination), input, gasU)
	case evmc.CallCode:
		output, gasLeftU, err = host.env.CallCode(host.contract, common.Address(destination), input, gasU, new(big.Int).SetBytes(value[:]))
	case evmc.Create:
		var createOutput []byte
		var createAddrCommon common.Address
		createOutput, createAddrCommon, gasLeftU, err = host.env.Create(host.contract, input, gasU, new(big.Int).SetBytes(value[:]))
		createAddr = evmc.Address(createAddrCommon)
		isHomestead := host.env.ChainConfig().IsHomestead(host.env.Context.BlockNumber)
		if !isHomestead && err == ErrCodeStoreOutOfGas {
			err = nil
		}
		if err == ErrExecutionReverted {
			// Assign return buffer from REVERT.
			// TODO: Bad API design: return data buffer and the code is returned in the same place. In worst case
			//       the code is returned also when there is not enough funds to deploy the code.
			output = createOutput
		}
	case evmc.Create2:
		var createOutput []byte
		var createAddrCommon common.Address
		u256Salt, _ := uint256.FromBig(new(big.Int).SetBytes(value[:]))

		createOutput, createAddrCommon, gasLeftU, err = host.env.Create2(host.contract, input, gasU, new(big.Int).SetBytes(value[:]), u256Salt)
		createAddr = evmc.Address(createAddrCommon)
		if err == ErrExecutionReverted {
			// Assign return buffer from REVERT.
			// TODO: Bad API design: return data buffer and the code is returned in the same place. In worst case
			//       the code is returned also when there is not enough funds to deploy the code.
			output = createOutput
		}
	default:
		panic(fmt.Errorf("EVMC: Unknown call kind %d", kind))
	}

	// Map errors.
	if err == ErrExecutionReverted {
		err = evmc.Revert
	} else if err != nil {
		err = evmc.Failure
	}

	gasRefund = int64(host.env.StateDB.GetRefund())
	gasLeft = int64(gasLeftU)
	return output, gasLeft, gasRefund, createAddr, err
}

func (host *hostContext) AccessAccount(addr evmc.Address) evmc.AccessStatus {
	if host.env.StateDB.AddressInAccessList(common.Address(addr)) {
		return evmc.WarmAccess
	} else {
		return evmc.ColdAccess
	}
}

func (host *hostContext) AccessStorage(addr evmc.Address, key evmc.Hash) evmc.AccessStatus {
	if _, slotPresent := host.env.StateDB.SlotInAccessList(common.Address(addr), common.Hash(key)); slotPresent {
		return evmc.WarmAccess
	} else {
		return evmc.ColdAccess
	}
}

// getRevision translates ChainConfig's HF block information into EVMC revision.
func getRevision(env *EVM) evmc.Revision {
	n := env.Context.BlockNumber
	conf := env.ChainConfig()
	switch {
	case conf.IsLondon(n):
		return evmc.London
	case conf.IsBerlin(n):
		return evmc.Berlin
	case conf.IsIstanbul(n):
		return evmc.Istanbul
	case conf.IsPetersburg(n):
		return evmc.Petersburg
	case conf.IsConstantinople(n):
		return evmc.Constantinople
	case conf.IsByzantium(n):
		return evmc.Byzantium
	case conf.IsEIP158(n):
		return evmc.SpuriousDragon
	case conf.IsEIP150(n):
		return evmc.TangerineWhistle
	case conf.IsHomestead(n):
		return evmc.Homestead
	default:
		return evmc.Frontier
	}
}

// Run implements Interpreter.Run().
func (evm *EVMC) Run(contract *Contract, input []byte, readOnly bool) (ret []byte, err error) {
	evm.env.depth++
	defer func() { evm.env.depth-- }()

	// Don't bother with the execution if there's no code.
	if len(contract.Code) == 0 {
		return nil, nil
	}

	kind := evmc.Call
	if evm.env.StateDB.GetCodeSize(contract.Address()) == 0 {
		// Guess if this is a CREATE.
		kind = evmc.Create
	}

	// Make sure the readOnly is only set if we aren't in readOnly yet.
	// This makes also sure that the readOnly flag isn't removed for child calls.
	if readOnly && !evm.readOnly {
		evm.readOnly = true
		defer func() { evm.readOnly = false }()
	}

	hc := hostContext{evm.env, contract}

	output, gasLeft, err := evm.instance.Execute(
		&hc,                              // HostContext
		getRevision(evm.env),             // Revision
		kind,                             // Kind
		evm.readOnly,                     // Static
		evm.env.depth-1,                  // Depth
		int64(contract.Gas),              // Gas
		evmc.Address(contract.Address()), // Recipient
		evmc.Address(contract.Caller()),  // Sender
		input,                            // Input
		evmc.Hash(common.BigToHash(contract.value)), // Value
		contract.Code) // Code

	contract.Gas = uint64(gasLeft)

	if err == evmc.Revert {
		err = ErrExecutionReverted
	} else if evmcError, ok := err.(evmc.Error); ok && evmcError.IsInternalError() {
		panic(fmt.Sprintf("EVMC VM internal error: %s", evmcError.Error()))
	}

	return output, err
}

// CanRun implements Interpreter.CanRun().
func (evm *EVMC) CanRun(code []byte) bool {
	required := evmc.CapabilityEVM1
	wasmPreamble := []byte("\x00asm")
	if bytes.HasPrefix(code, wasmPreamble) {
		required = evmc.CapabilityEWASM
	}
	return evm.cap == required
}
