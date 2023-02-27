package ibchooks_test

import (
	"encoding/json"
	"fmt"
	wasmkeeper "github.com/CosmWasm/wasmd/x/wasm/keeper"
	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/cosmos/cosmos-sdk/simapp/helpers"
	ibctesting "github.com/cosmos/interchain-security/legacy_ibc_testing/testing"
	"github.com/neutron-org/neutron/app/params"
	"github.com/neutron-org/neutron/testutil"
	ibchooks "github.com/neutron-org/neutron/x/ibc-hooks"
	ibchookskeeper "github.com/neutron-org/neutron/x/ibc-hooks/keeper"
	"github.com/neutron-org/neutron/x/ibc-hooks/testutils"
	"github.com/stretchr/testify/require"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	sdk "github.com/cosmos/cosmos-sdk/types"

	transfertypes "github.com/cosmos/ibc-go/v4/modules/apps/transfer/types"
	clienttypes "github.com/cosmos/ibc-go/v4/modules/core/02-client/types"
	channeltypes "github.com/cosmos/ibc-go/v4/modules/core/04-channel/types"
)

type HooksTestSuite struct {
	testutil.IBCConnectionTestSuite
}

func TestIBCHooksTestSuite(t *testing.T) {
	suite.Run(t, new(HooksTestSuite))
}

func (suite *HooksTestSuite) TestOnRecvPacketHooks() {
	var (
		trace    transfertypes.DenomTrace
		amount   sdk.Int
		receiver string
		status   testutils.Status
	)

	testCases := []struct {
		msg      string
		malleate func(*testutils.Status)
		expPass  bool
	}{
		{"override", func(status *testutils.Status) {
			suite.GetNeutronZoneApp(suite.ChainB).HooksTransferIBCModule.
				ICS4Middleware.Hooks = testutils.TestRecvOverrideHooks{Status: status}
		}, true},
		{"before and after", func(status *testutils.Status) {
			suite.GetNeutronZoneApp(suite.ChainB).HooksTransferIBCModule.
				ICS4Middleware.Hooks = testutils.TestRecvBeforeAfterHooks{Status: status}
		}, true},
	}

	for _, tc := range testCases {
		tc := tc
		suite.Run(tc.msg, func() {
			suite.SetupTest() // reset

			suite.ConfigureTransferChannel()
			receiver = suite.ChainB.SenderAccount.GetAddress().String() // must be explicitly changed in malleate
			status = testutils.Status{}

			amount = sdk.NewInt(100) // must be explicitly changed in malleate
			seq := uint64(1)

			trace = transfertypes.ParseDenomTrace(params.DefaultDenom)

			// send coin from chainA to chainB
			transferMsg := transfertypes.NewMsgTransfer(
				suite.TransferPath.EndpointA.ChannelConfig.PortID,
				suite.TransferPath.EndpointA.ChannelID,
				sdk.NewCoin(trace.IBCDenom(), amount),
				suite.ChainA.SenderAccount.GetAddress().String(),
				receiver,
				clienttypes.NewHeight(1, 110),
				0)
			_, err := suite.ChainA.SendMsgs(transferMsg)
			suite.Require().NoError(err) // message committed

			tc.malleate(&status)

			data := transfertypes.NewFungibleTokenPacketData(trace.GetFullDenomPath(), amount.String(), suite.ChainA.SenderAccount.GetAddress().String(), receiver)
			packet := channeltypes.NewPacket(data.GetBytes(), seq, suite.TransferPath.EndpointA.ChannelConfig.PortID, suite.TransferPath.EndpointA.ChannelID, suite.TransferPath.EndpointB.ChannelConfig.PortID, suite.TransferPath.EndpointB.ChannelID, clienttypes.NewHeight(1, 100), 0)

			ack := suite.GetNeutronZoneApp(suite.ChainB).HooksTransferIBCModule.
				OnRecvPacket(suite.ChainB.GetContext(), packet, suite.ChainA.SenderAccount.GetAddress())

			if tc.expPass {
				suite.Require().True(ack.Success())
			} else {
				suite.Require().False(ack.Success())
			}

			if _, ok := suite.GetNeutronZoneApp(suite.ChainB).HooksTransferIBCModule.
				ICS4Middleware.Hooks.(testutils.TestRecvOverrideHooks); ok {
				suite.Require().True(status.OverrideRan)
				suite.Require().False(status.BeforeRan)
				suite.Require().False(status.AfterRan)
			}

			if _, ok := suite.GetNeutronZoneApp(suite.ChainB).HooksTransferIBCModule.
				ICS4Middleware.Hooks.(testutils.TestRecvBeforeAfterHooks); ok {
				suite.Require().False(status.OverrideRan)
				suite.Require().True(status.BeforeRan)
				suite.Require().True(status.AfterRan)
			}
		})
	}
}

func (suite *HooksTestSuite) makeMockPacket(receiver, memo string, prevSequence uint64) channeltypes.Packet {
	packetData := transfertypes.FungibleTokenPacketData{
		Denom:    sdk.DefaultBondDenom,
		Amount:   "1",
		Sender:   suite.ChainB.SenderAccount.GetAddress().String(),
		Receiver: receiver,
		Memo:     memo,
	}

	return channeltypes.NewPacket(
		packetData.GetBytes(),
		prevSequence+1,
		suite.TransferPath.EndpointB.ChannelConfig.PortID,
		suite.TransferPath.EndpointB.ChannelID,
		suite.TransferPath.EndpointA.ChannelConfig.PortID,
		suite.TransferPath.EndpointA.ChannelID,
		clienttypes.NewHeight(0, 150),
		0,
	)
}

func (suite *HooksTestSuite) receivePacket(receiver, memo string) []byte {
	return suite.receivePacketWithSequence(receiver, memo, 0)
}

func (suite *HooksTestSuite) receivePacketWithSequence(receiver, memo string, prevSequence uint64) []byte {
	channelCap := suite.ChainB.GetChannelCapability(
		suite.TransferPath.EndpointB.ChannelConfig.PortID,
		suite.TransferPath.EndpointB.ChannelID)

	packet := suite.makeMockPacket(receiver, memo, prevSequence)

	err := suite.GetNeutronZoneApp(suite.ChainB).HooksICS4Wrapper.SendPacket(
		suite.ChainB.GetContext(), channelCap, packet)
	suite.Require().NoError(err, "IBC send failed. Expected success. %s", err)

	// Update both clients
	err = suite.TransferPath.EndpointB.UpdateClient()
	suite.Require().NoError(err)
	err = suite.TransferPath.EndpointA.UpdateClient()
	suite.Require().NoError(err)

	// recv in chain a
	res, err := suite.TransferPath.EndpointA.RecvPacketWithResult(packet)

	// get the ack from the chain a's response
	ack, err := ibctesting.ParseAckFromEvents(res.GetEvents())
	suite.Require().NoError(err)

	// manually send the acknowledgement to chain b
	err = suite.TransferPath.EndpointA.AcknowledgePacket(packet, ack)
	suite.Require().NoError(err)
	return ack
}

func (suite *HooksTestSuite) TestRecvTransferWithMetadata() {
	suite.ConfigureTransferChannel()

	// Setup contract
	codeId := suite.StoreContractCode(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), "./bytecode/echo.wasm")
	addr := suite.InstantiateContract(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), codeId, "{}")

	ackBytes := suite.receivePacket(addr.String(), fmt.Sprintf(`{"wasm": {"contract": "%s", "msg": {"echo": {"msg": "test"} } } }`, addr))
	ackStr := string(ackBytes)
	fmt.Println(ackStr)
	var ack map[string]string // This can't be unmarshalled to Acknowledgement because it's fetched from the events
	err := json.Unmarshal(ackBytes, &ack)
	suite.Require().NoError(err)
	suite.Require().NotContains(ack, "error")
	suite.Require().Equal(ack["result"], "eyJjb250cmFjdF9yZXN1bHQiOiJkR2hwY3lCemFHOTFiR1FnWldOb2J3PT0iLCJpYmNfYWNrIjoiZXlKeVpYTjFiSFFpT2lKQlVUMDlJbjA9In0=")
}

// After successfully executing a wasm call, the contract should have the funds sent via IBC
func (suite *HooksTestSuite) TestFundsAreTransferredToTheContract() {
	suite.ConfigureTransferChannel()

	// Setup contract
	codeId := suite.StoreContractCode(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), "./bytecode/echo.wasm")
	addr := suite.InstantiateContract(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), codeId, "{}")

	// Check that the contract has no funds
	localDenom := ibchooks.MustExtractDenomFromPacketOnRecv(suite.makeMockPacket("", "", 0))
	balance := suite.GetNeutronZoneApp(suite.ChainA).BankKeeper.GetBalance(suite.ChainA.GetContext(), addr, localDenom)
	suite.Require().Equal(sdk.NewInt(0), balance.Amount)

	// Execute the contract via IBC
	ackBytes := suite.receivePacket(addr.String(), fmt.Sprintf(`{"wasm": {"contract": "%s", "msg": {"echo": {"msg": "test"} } } }`, addr))
	ackStr := string(ackBytes)
	fmt.Println(ackStr)
	var ack map[string]string // This can't be unmarshalled to Acknowledgement because it's fetched from the events
	err := json.Unmarshal(ackBytes, &ack)
	suite.Require().NoError(err)
	suite.Require().NotContains(ack, "error")
	suite.Require().Equal(ack["result"], "eyJjb250cmFjdF9yZXN1bHQiOiJkR2hwY3lCemFHOTFiR1FnWldOb2J3PT0iLCJpYmNfYWNrIjoiZXlKeVpYTjFiSFFpT2lKQlVUMDlJbjA9In0=")

	// Check that the token has now been transferred to the contract
	balance = suite.GetNeutronZoneApp(suite.ChainA).BankKeeper.GetBalance(suite.ChainA.GetContext(), addr, localDenom)
	suite.Require().Equal(sdk.NewInt(1), balance.Amount)
}

// If the wasm call wails, the contract acknowledgement should be an error and the funds returned
func (suite *HooksTestSuite) TestFundsAreReturnedOnFailedContractExec() {
	suite.ConfigureTransferChannel()

	// Setup contract
	codeId := suite.StoreContractCode(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), "./bytecode/echo.wasm")
	addr := suite.InstantiateContract(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), codeId, "{}")

	// Check that the contract has no funds
	localDenom := ibchooks.MustExtractDenomFromPacketOnRecv(suite.makeMockPacket("", "", 0))
	balance := suite.GetNeutronZoneApp(suite.ChainA).BankKeeper.GetBalance(suite.ChainA.GetContext(), addr, localDenom)
	suite.Require().Equal(sdk.NewInt(0), balance.Amount)

	// Execute the contract via IBC with a message that the contract will reject
	ackBytes := suite.receivePacket(addr.String(), fmt.Sprintf(`{"wasm": {"contract": "%s", "msg": {"not_echo": {"msg": "test"} } } }`, addr))
	ackStr := string(ackBytes)
	fmt.Println(ackStr)
	var ack map[string]string // This can't be unmarshalled to Acknowledgement because it's fetched from the events
	err := json.Unmarshal(ackBytes, &ack)
	suite.Require().NoError(err)
	suite.Require().Contains(ack, "error")

	// Check that the token has now been transferred to the contract
	balance = suite.GetNeutronZoneApp(suite.ChainA).BankKeeper.GetBalance(suite.ChainA.GetContext(), addr, localDenom)
	fmt.Println(balance)
	suite.Require().Equal(sdk.NewInt(0), balance.Amount)
}

func (suite *HooksTestSuite) TestPacketsThatShouldBeSkipped() {
	suite.ConfigureTransferChannel()

	var sequence uint64
	receiver := suite.ChainB.SenderAccount.GetAddress().String()

	testCases := []struct {
		memo           string
		expPassthrough bool
	}{
		{"", true},
		{"{01]", true}, // bad json
		{"{}", true},
		{`{"something": ""}`, true},
		{`{"wasm": "test"}`, false},
		{`{"wasm": []`, true}, // invalid top level JSON
		{`{"wasm": {}`, true}, // invalid top level JSON
		{`{"wasm": []}`, false},
		{`{"wasm": {}}`, false},
		{`{"wasm": {"contract": "something"}}`, false},
		{`{"wasm": {"contract": "cosmos1clpqr4nrk4khgkxj78fcwwh6dl3uw4epasmvnj"}}`, false},
		{`{"wasm": {"msg": "something"}}`, false},
		// invalid receiver
		{`{"wasm": {"contract": "cosmos1clpqr4nrk4khgkxj78fcwwh6dl3uw4epasmvnj", "msg": {}}}`, false},
		// msg not an object
		{fmt.Sprintf(`{"wasm": {"contract": "%s", "msg": 1}}`, receiver), false},
	}

	for _, tc := range testCases {
		ackBytes := suite.receivePacketWithSequence(receiver, tc.memo, sequence)
		ackStr := string(ackBytes)
		//fmt.Println(ackStr)
		var ack map[string]string // This can't be unmarshalled to Acknowledgement because it's fetched from the events
		err := json.Unmarshal(ackBytes, &ack)
		suite.Require().NoError(err)
		if tc.expPassthrough {
			suite.Require().Equal("AQ==", ack["result"], tc.memo)
		} else {
			suite.Require().Contains(ackStr, "error", tc.memo)
		}
		sequence += 1
	}
}

// After successfully executing a wasm call, the contract should have the funds sent via IBC
func (suite *HooksTestSuite) TestFundTracking() {
	suite.ConfigureTransferChannel()

	// Setup contract
	codeId := suite.StoreContractCode(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), "./bytecode/counter.wasm")
	addr := suite.InstantiateContract(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), codeId, `{"count": 0}`)

	// Check that the contract has no funds
	localDenom := ibchooks.MustExtractDenomFromPacketOnRecv(suite.makeMockPacket("", "", 0))
	balance := suite.GetNeutronZoneApp(suite.ChainA).BankKeeper.GetBalance(suite.ChainA.GetContext(), addr, localDenom)
	suite.Require().Equal(sdk.NewInt(0), balance.Amount)

	// Execute the contract via IBC
	suite.receivePacket(
		addr.String(),
		fmt.Sprintf(`{"wasm": {"contract": "%s", "msg": {"increment": {} } } }`, addr))

	senderLocalAcc, err := ibchookskeeper.DeriveIntermediateSender(suite.TransferPath.EndpointB.ChannelID, suite.ChainB.SenderAccount.GetAddress().String(), "neutron")
	suite.Require().NoError(err)

	state := suite.QueryContract(
		suite.ChainA,
		addr,
		[]byte(fmt.Sprintf(`{"get_count": {"addr": "%s"}}`, senderLocalAcc)))
	suite.Require().Equal(`{"count":0}`, state)

	state = suite.QueryContract(
		suite.ChainA,
		addr,
		[]byte(fmt.Sprintf(`{"get_total_funds": {"addr": "%s"}}`, senderLocalAcc)))
	suite.Require().Equal(`{"total_funds":[{"denom":"ibc/D549749C93524DA1831A4B3C850DFC1BA9060261BEDFB224B3B0B4744CD77A70","amount":"1"}]}`, state)

	suite.receivePacketWithSequence(
		addr.String(),
		fmt.Sprintf(`{"wasm": {"contract": "%s", "msg": {"increment": {} } } }`, addr), 1)

	state = suite.QueryContract(
		suite.ChainA,
		addr,
		[]byte(fmt.Sprintf(`{"get_count": {"addr": "%s"}}`, senderLocalAcc)))
	suite.Require().Equal(`{"count":1}`, state)

	state = suite.QueryContract(
		suite.ChainA,
		addr,
		[]byte(fmt.Sprintf(`{"get_total_funds": {"addr": "%s"}}`, senderLocalAcc)))
	suite.Require().Equal(`{"total_funds":[{"denom":"ibc/D549749C93524DA1831A4B3C850DFC1BA9060261BEDFB224B3B0B4744CD77A70","amount":"2"}]}`, state)

	// Check that the token has now been transferred to the contract
	balance = suite.GetNeutronZoneApp(suite.ChainA).BankKeeper.GetBalance(suite.ChainA.GetContext(), addr, localDenom)
	suite.Require().Equal(sdk.NewInt(2), balance.Amount)
}

// custom MsgTransfer constructor that supports Memo
func NewMsgTransfer(
	sourcePort string, sourceChannel string, token sdk.Coin, sender, receiver string, memo string,
) *transfertypes.MsgTransfer {
	return &transfertypes.MsgTransfer{
		SourcePort:       sourcePort,
		SourceChannel:    sourceChannel,
		Token:            token,
		Sender:           sender,
		Receiver:         receiver,
		TimeoutHeight:    clienttypes.NewHeight(0, 100),
		TimeoutTimestamp: 0,
		Memo:             memo,
	}
}

type Direction int64

const (
	AtoB Direction = iota
	BtoA
)

func (suite *HooksTestSuite) GetEndpoints(direction Direction) (sender *ibctesting.Endpoint, receiver *ibctesting.Endpoint) {
	switch direction {
	case AtoB:
		sender = suite.TransferPath.EndpointA
		receiver = suite.TransferPath.EndpointB
	case BtoA:
		sender = suite.TransferPath.EndpointB
		receiver = suite.TransferPath.EndpointA
	}
	return sender, receiver
}

func (suite *HooksTestSuite) RelayPacket(packet channeltypes.Packet, direction Direction) (*sdk.Result, []byte) {
	sender, receiver := suite.GetEndpoints(direction)

	err := receiver.UpdateClient()
	suite.Require().NoError(err)

	// receiver Receives
	receiveResult, err := receiver.RecvPacketWithResult(packet)
	suite.Require().NoError(err)

	ack, err := ibctesting.ParseAckFromEvents(receiveResult.GetEvents())
	suite.Require().NoError(err)

	// sender Acknowledges
	err = sender.AcknowledgePacket(packet, ack)
	suite.Require().NoError(err)

	err = sender.UpdateClient()
	suite.Require().NoError(err)
	err = receiver.UpdateClient()
	suite.Require().NoError(err)

	return receiveResult, ack
}

func (suite *HooksTestSuite) FullSend(msg sdk.Msg, direction Direction) (*sdk.Result, *sdk.Result, string, error) {
	var sender *ibctesting.TestChain
	switch direction {
	case AtoB:
		sender = suite.ChainA
	case BtoA:
		sender = suite.ChainB
	}

	sendResult, err := SendMsgsNoCheck(sender, msg)
	suite.Require().NoError(err)

	packet, err := ParsePacketFromEvents(sendResult.GetEvents())
	suite.Require().NoError(err)

	receiveResult, ack := suite.RelayPacket(packet, direction)

	return sendResult, receiveResult, string(ack), err
}

func (suite *HooksTestSuite) TestAcks() {
	suite.ConfigureTransferChannel()

	// Setup contract
	codeId := suite.StoreContractCode(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), "./bytecode/counter.wasm")
	addr := suite.InstantiateContract(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), codeId, `{"count": 0}`)

	// предположение, что баг = когда мы выполняем SendPacket, мы не юзаем хук, а делаем это напрямую,
	// т.к. у нас customHandler?
	// Generate swap instructions for the contract
	callbackMemo := fmt.Sprintf(`{"ibc_callback":"%s"}`, addr)
	// Send IBC transfer with the memo with crosschain-swap instructions
	sourcePort := suite.TransferPath.EndpointA.ChannelConfig.PortID
	sourceChannel := suite.TransferPath.EndpointA.ChannelID
	transferMsg := NewMsgTransfer(sourcePort, sourceChannel, sdk.NewCoin(params.DefaultDenom, sdk.NewInt(1000)), suite.ChainA.SenderAccount.GetAddress().String(), addr.String(), callbackMemo)

	_, _, _, err := suite.FullSend(transferMsg, AtoB)
	require.NoError(suite.T(), err)

	// The test contract will increment the counter for itself every time it receives an ack
	state := suite.QueryContract(
		suite.ChainA,
		addr,
		[]byte(fmt.Sprintf(`{"get_count": {"addr": "%s"}}`, addr)))
	suite.Require().Equal(`{"count":1}`, state)

	_, _, _, err = suite.FullSend(transferMsg, AtoB)
	require.NoError(suite.T(), err)
	state = suite.QueryContract(suite.ChainA,
		addr,
		[]byte(fmt.Sprintf(`{"get_count": {"addr": "%s"}}`, addr)))
	suite.Require().Equal(`{"count":2}`, state)
}

func (suite *HooksTestSuite) TestTimeouts() {
	suite.ConfigureTransferChannel()

	// Setup contract
	codeId := suite.StoreContractCode(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), "./bytecode/counter.wasm")
	addr := suite.InstantiateContract(suite.ChainA, sdk.MustAccAddressFromBech32(testutil.TestOwnerAddress), codeId, `{"count": 0}`)

	// Generate swap instructions for the contract
	callbackMemo := fmt.Sprintf(`{"ibc_callback":"%s"}`, addr)
	// Send IBC transfer with the memo for ibc_callback
	sourcePort := suite.TransferPath.EndpointA.ChannelConfig.PortID
	sourceChannel := suite.TransferPath.EndpointA.ChannelID
	transferMsg := NewMsgTransfer(sourcePort, sourceChannel, sdk.NewCoin(params.DefaultDenom, sdk.NewInt(1000)), suite.ChainA.SenderAccount.GetAddress().String(), addr.String(), callbackMemo)
	transferMsg.TimeoutTimestamp = uint64(suite.Coordinator.CurrentTime.Add(time.Minute).UnixNano())
	sendResult, err := SendMsgsNoCheck(suite.ChainA, transferMsg)
	suite.Require().NoError(err)

	packet, err := ParsePacketFromEvents(sendResult.GetEvents())
	suite.Require().NoError(err)

	// Move chainB forward one block
	suite.ChainB.NextBlock()
	// One month later
	suite.Coordinator.IncrementTimeBy(time.Hour)
	err = suite.TransferPath.EndpointA.UpdateClient()
	suite.Require().NoError(err)

	err = suite.TransferPath.EndpointA.TimeoutPacket(packet)
	suite.Require().NoError(err)

	// The test contract will increment the counter for itself by 10 when a packet times out
	state := suite.QueryContract(
		suite.ChainA,
		addr,
		[]byte(fmt.Sprintf(`{"get_count": {"addr": "%s"}}`, addr)))
	suite.Require().Equal(`{"count":10}`, state)
}

func (suite *HooksTestSuite) TestSendWithoutMemo() {
	suite.ConfigureTransferChannel()

	// Sending a packet without memo to ensure that the ibc_callback middleware doesn't interfere with a regular send
	sourcePort := suite.TransferPath.EndpointA.ChannelConfig.PortID
	sourceChannel := suite.TransferPath.EndpointA.ChannelID
	transferMsg := NewMsgTransfer(sourcePort, sourceChannel, sdk.NewCoin(params.DefaultDenom, sdk.NewInt(1000)), suite.ChainA.SenderAccount.GetAddress().String(), suite.ChainA.SenderAccount.GetAddress().String(), "")
	_, _, ack, err := suite.FullSend(transferMsg, AtoB)
	suite.Require().NoError(err)
	suite.Require().Contains(ack, "result")
}

// SendMsgsNoCheck overrides ibctesting.TestChain.SendMsgs so that it doesn't check for errors. That should be handled by the caller
func SendMsgsNoCheck(chain *ibctesting.TestChain, msgs ...sdk.Msg) (*sdk.Result, error) {
	// ensure the chain has the latest time
	chain.Coordinator.UpdateTimeForChain(chain)

	_, r, err := SignAndDeliver(
		chain.TxConfig,
		chain.App.GetBaseApp(),
		chain.GetContext().BlockHeader(),
		msgs,
		chain.ChainID,
		[]uint64{chain.SenderAccount.GetAccountNumber()},
		[]uint64{chain.SenderAccount.GetSequence()},
		chain.SenderPrivKey,
	)
	if err != nil {
		return nil, err
	}

	// SignAndDeliver calls app.Commit()
	chain.NextBlock()

	// increment sequence for successful transaction execution
	err = chain.SenderAccount.SetSequence(chain.SenderAccount.GetSequence() + 1)
	if err != nil {
		return nil, err
	}

	chain.Coordinator.IncrementTime()

	return r, nil
}

// SignAndDeliver signs and delivers a transaction without asserting the results. This overrides the function
// from ibctesting
func SignAndDeliver(
	txCfg client.TxConfig, app *baseapp.BaseApp, header tmproto.Header, msgs []sdk.Msg,
	chainID string, accNums, accSeqs []uint64, priv ...cryptotypes.PrivKey,
) (sdk.GasInfo, *sdk.Result, error) {
	tx, _ := helpers.GenTx(
		txCfg,
		msgs,
		sdk.Coins{sdk.NewInt64Coin(params.DefaultDenom, 0)},
		helpers.DefaultGenTxGas,
		chainID,
		accNums,
		accSeqs,
		priv...,
	)

	// Simulate a sending a transaction and committing a block
	gInfo, res, err := app.Deliver(txCfg.TxEncoder(), tx)

	return gInfo, res, err
}

func (suite *HooksTestSuite) StoreContractCode(chain *ibctesting.TestChain, addr sdk.AccAddress, path string) uint64 {
	wasmCode, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}

	codeID, _, err := wasmkeeper.NewDefaultPermissionKeeper(suite.GetNeutronZoneApp(chain).WasmKeeper).Create(chain.GetContext(), addr, wasmCode, &wasmtypes.AccessConfig{Permission: wasmtypes.AccessTypeEverybody, Address: ""})
	if err != nil {
		panic(err)
	}

	return codeID
}

func (suite *HooksTestSuite) InstantiateContract(chain *ibctesting.TestChain, funder sdk.AccAddress, codeID uint64, initMsg string) sdk.AccAddress {
	contractKeeper := wasmkeeper.NewDefaultPermissionKeeper(suite.GetNeutronZoneApp(chain).WasmKeeper)
	addr, _, err := contractKeeper.Instantiate(chain.GetContext(), codeID, funder, funder, []byte(initMsg), "demo contract", nil)
	if err != nil {
		panic(err)
	}
	return addr
}

func (suite *HooksTestSuite) QueryContract(chain *ibctesting.TestChain, contract sdk.AccAddress, req []byte) string {
	state, err := suite.GetNeutronZoneApp(chain).WasmKeeper.QuerySmart(chain.GetContext(), contract, req)
	if err != nil {
		panic(err)
	}
	return string(state)
}

// ParsePacketFromEvents parses events emitted from a MsgRecvPacket and returns the
// acknowledgement.
func ParsePacketFromEvents(events sdk.Events) (channeltypes.Packet, error) {
	for _, ev := range events {
		if ev.Type == channeltypes.EventTypeSendPacket {
			packet := channeltypes.Packet{}
			for _, attr := range ev.Attributes {
				switch string(attr.Key) {
				case channeltypes.AttributeKeyData:
					packet.Data = attr.Value

				case channeltypes.AttributeKeySequence:
					seq, err := strconv.ParseUint(string(attr.Value), 10, 64)
					if err != nil {
						return channeltypes.Packet{}, err
					}

					packet.Sequence = seq

				case channeltypes.AttributeKeySrcPort:
					packet.SourcePort = string(attr.Value)

				case channeltypes.AttributeKeySrcChannel:
					packet.SourceChannel = string(attr.Value)

				case channeltypes.AttributeKeyDstPort:
					packet.DestinationPort = string(attr.Value)

				case channeltypes.AttributeKeyDstChannel:
					packet.DestinationChannel = string(attr.Value)

				case channeltypes.AttributeKeyTimeoutHeight:
					height, err := clienttypes.ParseHeight(string(attr.Value))
					if err != nil {
						return channeltypes.Packet{}, err
					}

					packet.TimeoutHeight = height

				case channeltypes.AttributeKeyTimeoutTimestamp:
					timestamp, err := strconv.ParseUint(string(attr.Value), 10, 64)
					if err != nil {
						return channeltypes.Packet{}, err
					}

					packet.TimeoutTimestamp = timestamp

				default:
					continue
				}
			}

			return packet, nil
		}
	}
	return channeltypes.Packet{}, fmt.Errorf("acknowledgement event attribute not found")
}
