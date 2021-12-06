// Copyright 2015 The go-ethereum Authors
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

package tests

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
)

func TestState(t *testing.T) {
	t.Parallel()

	st := new(testMatcher)
	// Long tests:
	st.slow(`^stAttackTest/ContractCreationSpam`)
	st.slow(`^stBadOpcode/badOpcodes`)
	st.slow(`^stPreCompiledContracts/modexp`)
	st.slow(`^stQuadraticComplexityTest/`)
	st.slow(`^stStaticCall/static_Call50000`)
	st.slow(`^stStaticCall/static_Return50000`)
	st.slow(`^stSystemOperationsTest/CallRecursiveBomb`)
	st.slow(`^stTransactionTest/Opcodes_TransactionInit`)

	// Very time consuming
	st.skipLoad(`^stTimeConsuming/`)
	st.skipLoad(`.*vmPerformance/loop.*`)

	// Uses 1GB RAM per tested fork
	st.skipLoad(`^stStaticCall/static_Call1MB`)

	// Broken tests:
	// Expected failures:
	//st.fails(`^stRevertTest/RevertPrecompiledTouch(_storage)?\.json/Byzantium/0`, "bug in test")
	//st.fails(`^stRevertTest/RevertPrecompiledTouch(_storage)?\.json/Byzantium/3`, "bug in test")
	//st.fails(`^stRevertTest/RevertPrecompiledTouch(_storage)?\.json/Constantinople/0`, "bug in test")
	//st.fails(`^stRevertTest/RevertPrecompiledTouch(_storage)?\.json/Constantinople/3`, "bug in test")
	//st.fails(`^stRevertTest/RevertPrecompiledTouch(_storage)?\.json/ConstantinopleFix/0`, "bug in test")
	//st.fails(`^stRevertTest/RevertPrecompiledTouch(_storage)?\.json/ConstantinopleFix/3`, "bug in test")

	// For Istanbul, older tests were moved into LegacyTests
	for _, dir := range []string{
		stateTestDir,
		legacyStateTestDir,
		benchmarksDir, // FIXME: This does not seem to work, but we want to test benchmarks!
	} {
		st.walk(t, dir, func(t *testing.T, name string, test *StateTest) {
			for _, subtest := range test.Subtests() {
				subtest := subtest
				key := fmt.Sprintf("%s/%d", subtest.Fork, subtest.Index)

				t.Run(key+"/trie", func(t *testing.T) {
					withTrace(t, test.gasLimit(subtest), func(vmconfig vm.Config) error {
						_, _, err := test.Run(subtest, vmconfig, false)
						if err != nil && len(test.json.Post[subtest.Fork][subtest.Index].ExpectException) > 0 {
							// Ignore expected errors (TODO MariusVanDerWijden check error string)
							return nil
						}
						return st.checkFailure(t, err)
					})
				})
				t.Run(key+"/snap", func(t *testing.T) {
					withTrace(t, test.gasLimit(subtest), func(vmconfig vm.Config) error {
						snaps, statedb, err := test.Run(subtest, vmconfig, true)
						if snaps != nil && statedb != nil {
							if _, err := snaps.Journal(statedb.IntermediateRoot(false)); err != nil {
								return err
							}
						}
						if err != nil && len(test.json.Post[subtest.Fork][subtest.Index].ExpectException) > 0 {
							// Ignore expected errors (TODO MariusVanDerWijden check error string)
							return nil
						}
						return st.checkFailure(t, err)
					})
				})
			}
		})
	}
}

func runBenchFunc(runTest interface{}, b *testing.B, name string, m reflect.Value, key string) {
	reflect.ValueOf(runTest).Call([]reflect.Value{
		reflect.ValueOf(b),
		reflect.ValueOf(name),
		m.MapIndex(reflect.ValueOf(key)),
	})
}

func makeMapFromBenchFunc(f interface{}) reflect.Value {
	stringT := reflect.TypeOf("")
	testingT := reflect.TypeOf((*testing.B)(nil))
	ftyp := reflect.TypeOf(f)
	if ftyp.Kind() != reflect.Func || ftyp.NumIn() != 3 || ftyp.NumOut() != 0 || ftyp.In(0) != testingT || ftyp.In(1) != stringT {
		panic(fmt.Sprintf("bad test function type: want func(*testing.T, string, <TestType>), have %s", ftyp))
	}
	testType := ftyp.In(2)
	mp := reflect.New(reflect.MapOf(stringT, testType))
	return mp.Elem()
}

func runBenchFile(b *testing.B, path, name string, runTest interface{}) {
	// Load the file as map[string]<testType>.
	m := makeMapFromBenchFunc(runTest)
	if err := readJSONFile(path, m.Addr().Interface()); err != nil {
		b.Fatal(err)
		return
	}

	// Run all tests from the map. Don't wrap in a subtest if there is only one test in the file.
	keys := sortedMapKeys(m)
	if len(keys) != 1 {
		b.Fatal("wrong number of keys")
		return
	}
	runBenchFunc(runTest, b, name, m, keys[0])
}

func benchWalk(b *testing.B, dir string, runTest interface{}) {
	// Walk the directory.
	dirinfo, err := os.Stat(dir)
	if os.IsNotExist(err) || !dirinfo.IsDir() {
		fmt.Fprintf(os.Stderr, "can't find test files in %s, did you clone the tests submodule?\n", dir)
		b.Skip("missing test files")
	}
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		name := filepath.ToSlash(strings.TrimPrefix(path, dir+string(filepath.Separator)))
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".json" {
			b.Run(name, func(b *testing.B) { runBenchFile(b, path, name, runTest) })
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
}

func BenchmarkState(b *testing.B) {
	{
		benchWalk(b, benchmarksDir, func(b *testing.B, name string, t *StateTest) {
			for _, subtest := range t.Subtests() {
				subtest := subtest
				key := fmt.Sprintf("%s/%d", subtest.Fork, subtest.Index)

				b.Run(key, func(b *testing.B) {
					vmconfig := vm.Config{}

					config, eips, err := GetChainConfig(subtest.Fork)
					if err != nil {
						b.Error(err)
						return
					}
					vmconfig.ExtraEips = eips
					block := t.genesis(config).ToBlock(nil)
					_, statedb := MakePreState(rawdb.NewMemoryDatabase(), t.json.Pre, false)

					var baseFee *big.Int
					if config.IsLondon(new(big.Int)) {
						baseFee = t.json.Env.BaseFee
						if baseFee == nil {
							// Retesteth uses `0x10` for genesis baseFee. Therefore, it defaults to
							// parent - 2 : 0xa as the basefee for 'this' context.
							baseFee = big.NewInt(0x0a)
						}
					}
					post := t.json.Post[subtest.Fork][subtest.Index]
					msg, err := t.json.Tx.toMessage(post, baseFee)
					if err != nil {
						b.Error(err)
						return
					}

					// Try to recover tx with current signer
					if len(post.TxBytes) != 0 {
						var ttx types.Transaction
						err := ttx.UnmarshalBinary(post.TxBytes)
						if err != nil {
							b.Error(err)
							return
						}

						if _, err := types.Sender(types.LatestSigner(config), &ttx); err != nil {
							b.Error(err)
							return
						}
					}

					// Prepare the EVM.
					txContext := core.NewEVMTxContext(msg)
					context := core.NewEVMBlockContext(block.Header(), nil, &t.json.Env.Coinbase)
					context.GetHash = vmTestBlockHash
					context.BaseFee = baseFee
					evm := vm.NewEVM(context, txContext, statedb, config, vmconfig)

					destAddr := msg.To()
					destAcc := vm.AccountRef(*destAddr)
					sender := vm.AccountRef(msg.From())

					// If the account has no code, we can abort here
					// The depth-check is already done, and precompiles handled above
					contract := vm.NewContract(sender, destAcc, msg.Value(), 0)
					contract.SetCallCode(destAddr, evm.StateDB.GetCodeHash(*destAddr), evm.StateDB.GetCode(*destAddr))

					interpreter := vm.NewEVMInterpreter(evm, vmconfig)

					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						// Execute the message.
						snapshot := statedb.Snapshot()

						contract.Gas = msg.Gas()
						_, err = interpreter.Run(contract, msg.Data(), false)


						//_, _, err = evm.Call(sender, *msg.To(), msg.Data(), msg.Gas(), msg.Value())

						if err != nil {
							b.Error(err)
							return
						}
						statedb.RevertToSnapshot(snapshot)
					}

				})
			}
		})
	}
}

// Transactions with gasLimit above this value will not get a VM trace on failure.
const traceErrorLimit = 400000

func withTrace(t *testing.T, gasLimit uint64, test func(vm.Config) error) {
	// Use config from command line arguments.
	config := vm.Config{}
	err := test(config)
	if err == nil {
		return
	}

	// Test failed, re-run with tracing enabled.
	t.Error(err)
	if gasLimit > traceErrorLimit {
		t.Log("gas limit too high for EVM trace")
		return
	}
	buf := new(bytes.Buffer)
	w := bufio.NewWriter(buf)
	tracer := logger.NewJSONLogger(&logger.Config{}, w)
	config.Debug, config.Tracer = true, tracer
	err2 := test(config)
	if !reflect.DeepEqual(err, err2) {
		t.Errorf("different error for second run: %v", err2)
	}
	w.Flush()
	if buf.Len() == 0 {
		t.Log("no EVM operation logs generated")
	} else {
		t.Log("EVM operation log:\n" + buf.String())
	}
	// t.Logf("EVM output: 0x%x", tracer.Output())
	// t.Logf("EVM error: %v", tracer.Error())
}
