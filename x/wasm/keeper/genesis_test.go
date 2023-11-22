package keeper

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	wasmvm "github.com/CosmWasm/wasmvm"
	dbm "github.com/cometbft/cometbft-db"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/libs/log"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	fuzz "github.com/google/gofuzz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/store"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authkeeper "github.com/cosmos/cosmos-sdk/x/auth/keeper"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	"github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	stakingkeeper "github.com/cosmos/cosmos-sdk/x/staking/keeper"

	"github.com/CosmWasm/wasmd/x/wasm/types"
)

func TestGenesisExportImport(t *testing.T) {
	wasmKeeper, srcCtx := setupKeeper(t)
	contractKeeper := NewGovPermissionKeeper(wasmKeeper)

	wasmCode, err := os.ReadFile("./testdata/hackatom.wasm")
	require.NoError(t, err)

	// store some test data
	f := fuzz.New().Funcs(ModelFuzzers...)

	err = wasmKeeper.SetParams(srcCtx, types.DefaultParams())
	require.NoError(t, err)

	for i := 0; i < 25; i++ {
		var (
			codeInfo          types.CodeInfo
			contract          types.ContractInfo
			stateModels       []types.Model
			history           []types.ContractCodeHistoryEntry
			pinned            bool
			contractExtension bool
		)
		f.Fuzz(&codeInfo)
		f.Fuzz(&contract)
		f.Fuzz(&stateModels)
		f.NilChance(0).Fuzz(&history)
		f.Fuzz(&pinned)
		f.Fuzz(&contractExtension)

		creatorAddr, err := sdk.AccAddressFromBech32(codeInfo.Creator)
		require.NoError(t, err)
		codeID, _, err := contractKeeper.Create(srcCtx, creatorAddr, wasmCode, &codeInfo.InstantiateConfig)
		require.NoError(t, err)
		if pinned {
			err = contractKeeper.PinCode(srcCtx, codeID)
			require.NoError(t, err)
		}
		if contractExtension {
			anyTime := time.Now().UTC()
			var nestedType v1beta1.TextProposal
			f.NilChance(0).Fuzz(&nestedType)
			myExtension, err := v1beta1.NewProposal(&nestedType, 1, anyTime, anyTime)
			require.NoError(t, err)
			err = contract.SetExtension(&myExtension)
			require.NoError(t, err)
		}

		contract.CodeID = codeID
		contractAddr := wasmKeeper.ClassicAddressGenerator()(srcCtx, codeID, nil)
		wasmKeeper.storeContractInfo(srcCtx, contractAddr, &contract)
		wasmKeeper.appendToContractHistory(srcCtx, contractAddr, history...)
		err = wasmKeeper.importContractState(srcCtx, contractAddr, stateModels)
		require.NoError(t, err)
	}
	var wasmParams types.Params
	f.NilChance(0).Fuzz(&wasmParams)
	err = wasmKeeper.SetParams(srcCtx, wasmParams)
	require.NoError(t, err)

	// export
	exportedState := ExportGenesis(srcCtx, wasmKeeper)
	// order should not matter
	rand.Shuffle(len(exportedState.Codes), func(i, j int) {
		exportedState.Codes[i], exportedState.Codes[j] = exportedState.Codes[j], exportedState.Codes[i]
	})
	rand.Shuffle(len(exportedState.Contracts), func(i, j int) {
		exportedState.Contracts[i], exportedState.Contracts[j] = exportedState.Contracts[j], exportedState.Contracts[i]
	})
	rand.Shuffle(len(exportedState.Sequences), func(i, j int) {
		exportedState.Sequences[i], exportedState.Sequences[j] = exportedState.Sequences[j], exportedState.Sequences[i]
	})
	exportedGenesis, err := wasmKeeper.cdc.MarshalJSON(exportedState)
	require.NoError(t, err)

	// setup new instances
	dstKeeper, dstCtx := setupKeeper(t)

	// reset contract code index in source DB for comparison with dest DB
	wasmKeeper.IterateContractInfo(srcCtx, func(address sdk.AccAddress, info types.ContractInfo) bool {
		creatorAddress := sdk.MustAccAddressFromBech32(info.Creator)
		history := wasmKeeper.GetContractHistory(srcCtx, address)

		wasmKeeper.addToContractCodeSecondaryIndex(srcCtx, address, history[len(history)-1])
		wasmKeeper.addToContractCreatorSecondaryIndex(srcCtx, creatorAddress, history[0].Updated, address)
		return false
	})

	originalMaxWasmSize := types.MaxWasmSize
	types.MaxWasmSize = 1

	// re-import
	var importState types.GenesisState
	err = dstKeeper.cdc.UnmarshalJSON(exportedGenesis, &importState)
	require.NoError(t, err)
	_, err = InitGenesis(dstCtx, dstKeeper, importState, TestHandler(contractKeeper))
	require.NoError(t, err)

	// compare whole DB

	srcIT := srcCtx.KVStore(wasmKeeper.storeKey).Iterator(nil, nil)
	dstIT := dstCtx.KVStore(dstKeeper.storeKey).Iterator(nil, nil)

	t.Cleanup(func() {
		types.MaxWasmSize = originalMaxWasmSize
		srcIT.Close()
		dstIT.Close()
	})

	for i := 0; srcIT.Valid(); i++ {
		require.True(t, dstIT.Valid(), "[%s] destination DB has less elements than source. Missing: %x", wasmKeeper.storeKey.Name(), srcIT.Key())
		require.Equal(t, srcIT.Key(), dstIT.Key(), i)
		require.Equal(t, srcIT.Value(), dstIT.Value(), "[%s] element (%d): %X", wasmKeeper.storeKey.Name(), i, srcIT.Key())
		dstIT.Next()
		srcIT.Next()
	}
	if !assert.False(t, dstIT.Valid()) {
		t.Fatalf("dest Iterator still has key :%X", dstIT.Key())
	}
}

func TestGenesisExportImportWithPredictableAddress(t *testing.T) {
	ctx, keepers := CreateTestInput(t, false, AvailableCapabilities)
	k := keepers.WasmKeeper
	contractKeeper := NewGovPermissionKeeper(k)
	eCtx, _ := ctx.CacheContext()
	codeID := StoreReflectContract(t, eCtx, keepers).CodeID
	creator := RandomAccountAddress(t)
	_, _, err := keepers.ContractKeeper.Instantiate2(eCtx, codeID, creator, nil, []byte("{}"), "testing", nil, []byte("my_salt"), false)
	require.NoError(t, err)
	genesisState := ExportGenesis(eCtx, k)
	// when imported
	_, err = InitGenesis(ctx, k, *genesisState, TestHandler(contractKeeper))
	require.NoError(t, err)
}

func TestGenesisInit(t *testing.T) {
	wasmCode, err := os.ReadFile("./testdata/hackatom.wasm")
	require.NoError(t, err)

	myCodeInfo := types.CodeInfoFixture(types.WithSHA256CodeHash(wasmCode))
	specs := map[string]struct {
		src            types.GenesisState
		stakingMock    StakingKeeperMock
		msgHandlerMock MockMsgHandler
		expSuccess     bool
	}{
		"happy path: code info correct": {
			src: types.GenesisState{
				Codes: []types.Code{{
					CodeID:    1,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				}},
				Sequences: []types.Sequence{
					{IDKey: types.KeySequenceCodeID, Value: 2},
					{IDKey: types.KeySequenceInstanceID, Value: 1},
				},
				Params: types.DefaultParams(),
			},
			expSuccess: true,
		},
		"happy path: code ids can contain gaps": {
			src: types.GenesisState{
				Codes: []types.Code{{
					CodeID:    1,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				}, {
					CodeID:    3,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				}},
				Sequences: []types.Sequence{
					{IDKey: types.KeySequenceCodeID, Value: 10},
					{IDKey: types.KeySequenceInstanceID, Value: 1},
				},
				Params: types.DefaultParams(),
			},
			expSuccess: true,
		},
		"happy path: code order does not matter": {
			src: types.GenesisState{
				Codes: []types.Code{{
					CodeID:    2,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				}, {
					CodeID:    1,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				}},
				Contracts: nil,
				Sequences: []types.Sequence{
					{IDKey: types.KeySequenceCodeID, Value: 3},
					{IDKey: types.KeySequenceInstanceID, Value: 1},
				},
				Params: types.DefaultParams(),
			},
			expSuccess: true,
		},
		"prevent code hash mismatch": {src: types.GenesisState{
			Codes: []types.Code{{
				CodeID:    1,
				CodeInfo:  types.CodeInfoFixture(func(i *types.CodeInfo) { i.CodeHash = make([]byte, sha256.Size) }),
				CodeBytes: wasmCode,
			}},
			Params: types.DefaultParams(),
		}},
		"prevent duplicate codeIDs": {src: types.GenesisState{
			Codes: []types.Code{
				{
					CodeID:    1,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				},
				{
					CodeID:    1,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				},
			},
			Params: types.DefaultParams(),
		}},
		"codes with same checksum can be pinned": {
			src: types.GenesisState{
				Codes: []types.Code{
					{
						CodeID:    1,
						CodeInfo:  myCodeInfo,
						CodeBytes: wasmCode,
						Pinned:    true,
					},
					{
						CodeID:    2,
						CodeInfo:  myCodeInfo,
						CodeBytes: wasmCode,
						Pinned:    true,
					},
				},
				Params: types.DefaultParams(),
			},
		},
		"happy path: code id in info and contract do match": {
			src: types.GenesisState{
				Codes: []types.Code{{
					CodeID:    1,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				}},
				Contracts: []types.Contract{
					{
						ContractAddress: BuildContractAddressClassic(1, 1).String(),
						ContractInfo:    types.ContractInfoFixture(func(c *types.ContractInfo) { c.CodeID = 1 }, types.RandCreatedFields),
						ContractCodeHistory: []types.ContractCodeHistoryEntry{
							{
								Operation: types.ContractCodeHistoryOperationTypeMigrate,
								CodeID:    1,
								Updated:   &types.AbsoluteTxPosition{BlockHeight: rand.Uint64(), TxIndex: rand.Uint64()},
								Msg:       []byte(`{}`),
							},
						},
					},
				},
				Sequences: []types.Sequence{
					{IDKey: types.KeySequenceCodeID, Value: 2},
					{IDKey: types.KeySequenceInstanceID, Value: 2},
				},
				Params: types.DefaultParams(),
			},
			expSuccess: true,
		},
		"happy path: code info with two contracts": {
			src: types.GenesisState{
				Codes: []types.Code{{
					CodeID:    1,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				}},
				Contracts: []types.Contract{
					{
						ContractAddress: BuildContractAddressClassic(1, 1).String(),
						ContractInfo:    types.ContractInfoFixture(func(c *types.ContractInfo) { c.CodeID = 1 }, types.RandCreatedFields),
						ContractCodeHistory: []types.ContractCodeHistoryEntry{
							{
								Operation: types.ContractCodeHistoryOperationTypeMigrate,
								CodeID:    1,
								Updated:   &types.AbsoluteTxPosition{BlockHeight: rand.Uint64(), TxIndex: rand.Uint64()},
								Msg:       []byte(`{}`),
							},
						},
					}, {
						ContractAddress: BuildContractAddressClassic(1, 2).String(),
						ContractInfo:    types.ContractInfoFixture(func(c *types.ContractInfo) { c.CodeID = 1 }, types.RandCreatedFields),
						ContractCodeHistory: []types.ContractCodeHistoryEntry{
							{
								Operation: types.ContractCodeHistoryOperationTypeMigrate,
								CodeID:    1,
								Updated:   &types.AbsoluteTxPosition{BlockHeight: rand.Uint64(), TxIndex: rand.Uint64()},
								Msg:       []byte(`{"foo":"bar"}`),
							},
						},
					},
				},
				Sequences: []types.Sequence{
					{IDKey: types.KeySequenceCodeID, Value: 2},
					{IDKey: types.KeySequenceInstanceID, Value: 3},
				},
				Params: types.DefaultParams(),
			},
			expSuccess: true,
		},
		"prevent contracts that points to non existing codeID": {
			src: types.GenesisState{
				Contracts: []types.Contract{
					{
						ContractAddress: BuildContractAddressClassic(1, 1).String(),
						ContractInfo:    types.ContractInfoFixture(func(c *types.ContractInfo) { c.CodeID = 1 }, types.RandCreatedFields),
						ContractCodeHistory: []types.ContractCodeHistoryEntry{
							{
								Operation: types.ContractCodeHistoryOperationTypeMigrate,
								CodeID:    1,
								Updated:   &types.AbsoluteTxPosition{BlockHeight: rand.Uint64(), TxIndex: rand.Uint64()},
								Msg:       []byte(`{"foo":"bar"}`),
							},
						},
					},
				},
				Params: types.DefaultParams(),
			},
		},
		"prevent duplicate contract address": {
			src: types.GenesisState{
				Codes: []types.Code{{
					CodeID:    1,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				}},
				Contracts: []types.Contract{
					{
						ContractAddress: BuildContractAddressClassic(1, 1).String(),
						ContractInfo:    types.ContractInfoFixture(func(c *types.ContractInfo) { c.CodeID = 1 }, types.RandCreatedFields),
						ContractCodeHistory: []types.ContractCodeHistoryEntry{
							{
								Operation: types.ContractCodeHistoryOperationTypeMigrate,
								CodeID:    1,
								Updated:   &types.AbsoluteTxPosition{BlockHeight: rand.Uint64(), TxIndex: rand.Uint64()},
								Msg:       []byte(`{"foo":"bar"}`),
							},
						},
					}, {
						ContractAddress: BuildContractAddressClassic(1, 1).String(),
						ContractInfo:    types.ContractInfoFixture(func(c *types.ContractInfo) { c.CodeID = 1 }, types.RandCreatedFields),
						ContractCodeHistory: []types.ContractCodeHistoryEntry{
							{
								Operation: types.ContractCodeHistoryOperationTypeMigrate,
								CodeID:    1,
								Updated:   &types.AbsoluteTxPosition{BlockHeight: rand.Uint64(), TxIndex: rand.Uint64()},
								Msg:       []byte(`{"other":"value"}`),
							},
						},
					},
				},
				Params: types.DefaultParams(),
			},
		},
		"prevent duplicate contract model keys": {
			src: types.GenesisState{
				Codes: []types.Code{{
					CodeID:    1,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				}},
				Contracts: []types.Contract{
					{
						ContractAddress: BuildContractAddressClassic(1, 1).String(),
						ContractInfo:    types.ContractInfoFixture(func(c *types.ContractInfo) { c.CodeID = 1 }, types.RandCreatedFields),
						ContractState: []types.Model{
							{
								Key:   []byte{0x1},
								Value: []byte("foo"),
							},
							{
								Key:   []byte{0x1},
								Value: []byte("bar"),
							},
						},
						ContractCodeHistory: []types.ContractCodeHistoryEntry{
							{
								Operation: types.ContractCodeHistoryOperationTypeMigrate,
								CodeID:    1,
								Updated:   &types.AbsoluteTxPosition{BlockHeight: rand.Uint64(), TxIndex: rand.Uint64()},
								Msg:       []byte(`{"foo":"bar"}`),
							},
						},
					},
				},
				Params: types.DefaultParams(),
			},
		},
		"prevent duplicate sequences": {
			src: types.GenesisState{
				Sequences: []types.Sequence{
					{IDKey: []byte("foo"), Value: 1},
					{IDKey: []byte("foo"), Value: 9999},
				},
				Params: types.DefaultParams(),
			},
		},
		"prevent code id seq init value == max codeID used": {
			src: types.GenesisState{
				Codes: []types.Code{{
					CodeID:    2,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				}},
				Sequences: []types.Sequence{
					{IDKey: types.KeySequenceCodeID, Value: 1},
				},
				Params: types.DefaultParams(),
			},
		},
		"prevent contract id seq init value not high enough": {
			src: types.GenesisState{
				Codes: []types.Code{{
					CodeID:    1,
					CodeInfo:  myCodeInfo,
					CodeBytes: wasmCode,
				}},
				Contracts: []types.Contract{
					{
						ContractAddress: BuildContractAddressClassic(1, 1).String(),
						ContractInfo:    types.ContractInfoFixture(func(c *types.ContractInfo) { c.CodeID = 1 }, types.RandCreatedFields),
						ContractCodeHistory: []types.ContractCodeHistoryEntry{
							{
								Operation: types.ContractCodeHistoryOperationTypeMigrate,
								CodeID:    1,
								Updated:   &types.AbsoluteTxPosition{BlockHeight: rand.Uint64(), TxIndex: rand.Uint64()},
								Msg:       []byte(`{}`),
							},
						},
					},
				},
				Sequences: []types.Sequence{
					{IDKey: types.KeySequenceCodeID, Value: 2},
					{IDKey: types.KeySequenceInstanceID, Value: 1},
				},
				Params: types.DefaultParams(),
			},
		},
		"validator set update called for any genesis messages": {
			src: types.GenesisState{
				GenMsgs: []types.GenesisState_GenMsgs{
					{Sum: &types.GenesisState_GenMsgs_StoreCode{
						StoreCode: types.MsgStoreCodeFixture(),
					}},
				},
				Params: types.DefaultParams(),
			},
			stakingMock:    StakingKeeperMock{expCalls: 0, validatorUpdate: []abci.ValidatorUpdate(nil)},
			msgHandlerMock: MockMsgHandler{expCalls: 1, expMsg: types.MsgStoreCodeFixture()},
			expSuccess:     true,
		},
		"validator set update not called on genesis msg handler errors": {
			src: types.GenesisState{
				GenMsgs: []types.GenesisState_GenMsgs{
					{Sum: &types.GenesisState_GenMsgs_StoreCode{
						StoreCode: types.MsgStoreCodeFixture(),
					}},
				},
				Params: types.DefaultParams(),
			},
			msgHandlerMock: MockMsgHandler{expCalls: 1, err: errors.New("test error response")},
			stakingMock:    StakingKeeperMock{expCalls: 0},
		},
	}
	for msg, spec := range specs {
		t.Run(msg, func(t *testing.T) {
			keeper, ctx := setupKeeper(t)

			require.NoError(t, types.ValidateGenesis(spec.src))
			gotValidatorSet, gotErr := InitGenesis(ctx, keeper, spec.src, &spec.msgHandlerMock)
			if !spec.expSuccess {
				require.Error(t, gotErr)
				return
			}
			require.NoError(t, gotErr)
			spec.msgHandlerMock.verifyCalls(t)
			spec.stakingMock.verifyCalls(t)
			assert.Equal(t, spec.stakingMock.validatorUpdate, gotValidatorSet)
			for _, c := range spec.src.Codes {
				assert.Equal(t, c.Pinned, keeper.IsPinnedCode(ctx, c.CodeID))
			}
		})
	}
}

func TestImportContractWithCodeHistoryPreserved(t *testing.T) {
	genesisTemplate := `
{
	"params":{
		"code_upload_access": {
			"permission": "Everybody"
		},
		"instantiate_default_permission": "Everybody"
	},
  "codes": [
    {
      "code_id": "1",
      "code_info": {
        "code_hash": %q,
        "creator": "cosmos1qtu5n0cnhfkjj6l2rq97hmky9fd89gwca9yarx",
        "instantiate_config": {
          "permission": "AnyOfAddresses",
          "addresses": ["cosmos1qtu5n0cnhfkjj6l2rq97hmky9fd89gwca9yarx"]
        }
      },
      "code_bytes": %q
    }
  ],
  "contracts": [
    {
      "contract_address": "cosmos14hj2tavq8fpesdwxxcu44rty3hh90vhujrvcmstl4zr3txmfvw9s4hmalr",
      "contract_info": {
        "code_id": "1",
        "creator": "cosmos13x849jzd03vne42ynpj25hn8npjecxqrjghd8x",
        "admin": "cosmos1h5t8zxmjr30e9dqghtlpl40f2zz5cgey6esxtn",
        "label": "ȀĴnZV芢毤",
		"created": {
			"block_height" : "100",
			"tx_index" : "10"
		}
      },
	  "contract_code_history": [
		{
			"operation": "CONTRACT_CODE_HISTORY_OPERATION_TYPE_INIT",
			"code_id": "1",
			"updated": {
				"block_height" : "100",
				"tx_index" : "10"
			},
			"msg": {"foo": "bar"}
	  	},
		{
			"operation": "CONTRACT_CODE_HISTORY_OPERATION_TYPE_MIGRATE",
			"code_id": "1",
			"updated": {
				"block_height" : "200",
				"tx_index" : "10"
			},
			"msg": {"other": "msg"}
	  	}
		]
    }
  ],
  "sequences": [
  {"id_key": "BGxhc3RDb2RlSWQ=", "value": "2"},
  {"id_key": "BGxhc3RDb250cmFjdElk", "value": "3"}
  ]
}`
	keeper, ctx := setupKeeper(t)
	contractKeeper := NewGovPermissionKeeper(keeper)

	wasmCode, err := os.ReadFile("./testdata/hackatom.wasm")
	require.NoError(t, err)

	wasmCodeHash, err := wasmvm.CreateChecksum(wasmCode)
	require.NoError(t, err)
	enc64 := base64.StdEncoding.EncodeToString
	genesisStr := fmt.Sprintf(genesisTemplate, enc64(wasmCodeHash[:]), enc64(wasmCode))

	var importState types.GenesisState
	err = keeper.cdc.UnmarshalJSON([]byte(genesisStr), &importState)
	require.NoError(t, err)
	require.NoError(t, importState.ValidateBasic(), genesisStr)

	ctx = ctx.WithBlockHeight(0).WithGasMeter(sdk.NewInfiniteGasMeter())

	// when
	_, err = InitGenesis(ctx, keeper, importState, TestHandler(contractKeeper))
	require.NoError(t, err)

	// verify wasm code
	gotWasmCode, err := keeper.GetByteCode(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, wasmCode, gotWasmCode, "byte code does not match")

	// verify code info
	gotCodeInfo := keeper.GetCodeInfo(ctx, 1)
	require.NotNil(t, gotCodeInfo)
	codeCreatorAddr := "cosmos1qtu5n0cnhfkjj6l2rq97hmky9fd89gwca9yarx"
	expCodeInfo := types.CodeInfo{
		CodeHash: wasmCodeHash[:],
		Creator:  codeCreatorAddr,
		InstantiateConfig: types.AccessConfig{
			Permission: types.AccessTypeAnyOfAddresses,
			Addresses:  []string{codeCreatorAddr},
		},
	}
	assert.Equal(t, expCodeInfo, *gotCodeInfo)

	// verify contract
	contractAddr, _ := sdk.AccAddressFromBech32("cosmos14hj2tavq8fpesdwxxcu44rty3hh90vhujrvcmstl4zr3txmfvw9s4hmalr")
	gotContractInfo := keeper.GetContractInfo(ctx, contractAddr)
	require.NotNil(t, gotContractInfo)
	contractCreatorAddr := "cosmos13x849jzd03vne42ynpj25hn8npjecxqrjghd8x"
	adminAddr := "cosmos1h5t8zxmjr30e9dqghtlpl40f2zz5cgey6esxtn"

	expContractInfo := types.ContractInfo{
		CodeID:  1,
		Creator: contractCreatorAddr,
		Admin:   adminAddr,
		Label:   "ȀĴnZV芢毤",
		Created: &types.AbsoluteTxPosition{BlockHeight: 100, TxIndex: 10},
	}
	assert.Equal(t, expContractInfo, *gotContractInfo)

	expHistory := []types.ContractCodeHistoryEntry{
		{
			Operation: types.ContractCodeHistoryOperationTypeInit,
			CodeID:    1,
			Updated: &types.AbsoluteTxPosition{
				BlockHeight: 100,
				TxIndex:     10,
			},
			Msg: []byte(`{"foo": "bar"}`),
		},
		{
			Operation: types.ContractCodeHistoryOperationTypeMigrate,
			CodeID:    1,
			Updated: &types.AbsoluteTxPosition{
				BlockHeight: 200,
				TxIndex:     10,
			},
			Msg: []byte(`{"other": "msg"}`),
		},
	}
	assert.Equal(t, expHistory, keeper.GetContractHistory(ctx, contractAddr))
	assert.Equal(t, uint64(2), keeper.PeekAutoIncrementID(ctx, types.KeySequenceCodeID))
	assert.Equal(t, uint64(3), keeper.PeekAutoIncrementID(ctx, types.KeySequenceInstanceID))
}

func TestSupportedGenMsgTypes(t *testing.T) {
	wasmCode, err := os.ReadFile("./testdata/hackatom.wasm")
	require.NoError(t, err)
	var (
		myAddress          sdk.AccAddress = bytes.Repeat([]byte{1}, types.ContractAddrLen)
		verifierAddress    sdk.AccAddress = bytes.Repeat([]byte{2}, types.ContractAddrLen)
		beneficiaryAddress sdk.AccAddress = bytes.Repeat([]byte{3}, types.ContractAddrLen)
	)
	const denom = "stake"
	importState := types.GenesisState{
		Params: types.DefaultParams(),
		GenMsgs: []types.GenesisState_GenMsgs{
			{
				Sum: &types.GenesisState_GenMsgs_StoreCode{
					StoreCode: &types.MsgStoreCode{
						Sender:       myAddress.String(),
						WASMByteCode: wasmCode,
					},
				},
			},
			{
				Sum: &types.GenesisState_GenMsgs_InstantiateContract{
					InstantiateContract: &types.MsgInstantiateContract{
						Sender: myAddress.String(),
						CodeID: 1,
						Label:  "testing",
						Msg: HackatomExampleInitMsg{
							Verifier:    verifierAddress,
							Beneficiary: beneficiaryAddress,
						}.GetBytes(t),
						Funds: sdk.NewCoins(sdk.NewCoin(denom, sdk.NewInt(10))),
					},
				},
			},
			{
				Sum: &types.GenesisState_GenMsgs_ExecuteContract{
					ExecuteContract: &types.MsgExecuteContract{
						Sender:   verifierAddress.String(),
						Contract: BuildContractAddressClassic(1, 1).String(),
						Msg:      []byte(`{"release":{}}`),
					},
				},
			},
		},
	}
	require.NoError(t, importState.ValidateBasic())
	ctx, keepers := CreateDefaultTestInput(t)
	keeper := keepers.WasmKeeper
	ctx = ctx.WithBlockHeight(0).WithGasMeter(sdk.NewInfiniteGasMeter())
	keepers.Faucet.Fund(ctx, myAddress, sdk.NewCoin(denom, sdk.NewInt(100)))

	// when
	_, err = InitGenesis(ctx, keeper, importState, TestHandler(keepers.ContractKeeper))
	require.NoError(t, err)

	// verify code stored
	gotWasmCode, err := keeper.GetByteCode(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, wasmCode, gotWasmCode)
	codeInfo := keeper.GetCodeInfo(ctx, 1)
	require.NotNil(t, codeInfo)

	// verify contract instantiated
	cInfo := keeper.GetContractInfo(ctx, BuildContractAddressClassic(1, 1))
	require.NotNil(t, cInfo)

	// verify contract executed
	gotBalance := keepers.BankKeeper.GetBalance(ctx, beneficiaryAddress, denom)
	assert.Equal(t, sdk.NewCoin(denom, sdk.NewInt(10)), gotBalance)
}

func setupKeeper(t *testing.T) (*Keeper, sdk.Context) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "wasm")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(tempDir) })

	keyWasm := sdk.NewKVStoreKey(types.StoreKey)

	db := dbm.NewMemDB()
	ms := store.NewCommitMultiStore(db)
	ms.MountStoreWithDB(keyWasm, storetypes.StoreTypeIAVL, db)
	require.NoError(t, ms.LoadLatestVersion())

	ctx := sdk.NewContext(ms, tmproto.Header{
		Height: 1234567,
		Time:   time.Date(2020, time.April, 22, 12, 0, 0, 0, time.UTC),
	}, false, log.NewNopLogger())

	encodingConfig := MakeEncodingConfig(t)
	// register an example extension. must be protobuf
	encodingConfig.InterfaceRegistry.RegisterImplementations(
		(*types.ContractInfoExtension)(nil),
		&v1beta1.Proposal{},
	)
	// also registering gov interfaces for nested Any type
	v1beta1.RegisterInterfaces(encodingConfig.InterfaceRegistry)

	wasmConfig := types.DefaultWasmConfig()

	srcKeeper := NewKeeper(
		encodingConfig.Codec,
		keyWasm,
		authkeeper.AccountKeeper{},
		&bankkeeper.BaseKeeper{},
		stakingkeeper.Keeper{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		tempDir,
		wasmConfig,
		AvailableCapabilities,
		authtypes.NewModuleAddress(govtypes.ModuleName).String(),
	)
	return &srcKeeper, ctx
}

var _ MessageRouter = &MockMsgHandler{}

func (m *MockMsgHandler) Handler(msg sdk.Msg) baseapp.MsgServiceHandler {
	return m.Handle
}

type StakingKeeperMock struct {
	err             error
	validatorUpdate []abci.ValidatorUpdate
	expCalls        int
	gotCalls        int
}

func (s *StakingKeeperMock) ApplyAndReturnValidatorSetUpdates(_ sdk.Context) ([]abci.ValidatorUpdate, error) {
	s.gotCalls++
	return s.validatorUpdate, s.err
}

func (s *StakingKeeperMock) verifyCalls(t *testing.T) {
	assert.Equal(t, s.expCalls, s.gotCalls, "number calls")
}

type MockMsgHandler struct {
	result   *sdk.Result
	err      error
	expCalls int
	gotCalls int
	expMsg   sdk.Msg
	gotMsg   sdk.Msg
}

func (m *MockMsgHandler) Handle(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
	m.gotCalls++
	m.gotMsg = msg
	return m.result, m.err
}

func (m *MockMsgHandler) verifyCalls(t *testing.T) {
	assert.Equal(t, m.expMsg, m.gotMsg, "message param")
	assert.Equal(t, m.expCalls, m.gotCalls, "number calls")
}
