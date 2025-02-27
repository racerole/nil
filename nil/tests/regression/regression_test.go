package main

import (
	"testing"

	"github.com/NilFoundation/nil/nil/common"
	"github.com/NilFoundation/nil/nil/common/hexutil"
	"github.com/NilFoundation/nil/nil/internal/contracts"
	"github.com/NilFoundation/nil/nil/internal/execution"
	"github.com/NilFoundation/nil/nil/internal/types"
	"github.com/NilFoundation/nil/nil/services/nilservice"
	"github.com/NilFoundation/nil/nil/services/rpc"
	"github.com/NilFoundation/nil/nil/tests"
	"github.com/stretchr/testify/suite"
)

type SuiteRegression struct {
	tests.RpcSuite

	zerostateCfg string
	testAddress  types.Address
}

func (s *SuiteRegression) SetupSuite() {
	s.ShardsNum = 4

	var err error
	s.testAddress, err = contracts.CalculateAddress(contracts.NameTest, 1, []byte{1})
	s.Require().NoError(err)

	zerostateTmpl := `
contracts:
- name: MainSmartAccount
  address: {{ .MainSmartAccountAddress }}
  value: 10000000000000000000
  contract: SmartAccount
  ctorArgs: [{{ .MainPublicKey }}]
- name: Test
  address: {{ .TestAddress }}
  value: 100000000000000
  contract: tests/Test
`
	s.zerostateCfg, err = common.ParseTemplate(zerostateTmpl, map[string]interface{}{
		"MainPublicKey":           hexutil.Encode(execution.MainPublicKey),
		"MainSmartAccountAddress": types.MainSmartAccountAddress.Hex(),
		"TestAddress":             s.testAddress.Hex(),
	})
	s.Require().NoError(err)
}

func (s *SuiteRegression) SetupTest() {
	zeroStateConfig, err := execution.ParseZeroStateConfig(s.zerostateCfg)
	s.Require().NoError(err)
	zeroStateConfig.MainPublicKey = execution.MainPublicKey

	s.Start(&nilservice.Config{
		NShards:   s.ShardsNum,
		HttpUrl:   rpc.GetSockPath(s.T()),
		RunMode:   nilservice.CollatorsOnlyRunMode,
		ZeroState: zeroStateConfig,
	})
	tests.WaitShardTick(s.T(), s.Context, s.Client, types.MainShardId)
	tests.WaitShardTick(s.T(), s.Context, s.Client, types.BaseShardId)
}

func (s *SuiteRegression) TearDownTest() {
	s.Cancel()
}

func (s *SuiteRegression) TestStaticCall() {
	code, err := contracts.GetCode("tests/StaticCallSource")
	s.Require().NoError(err)
	payload := types.BuildDeployPayload(code, common.EmptyHash)

	addrSource, receipt := s.DeployContractViaMainSmartAccount(types.BaseShardId, payload, types.GasToValue(10_000_000))
	s.Require().True(receipt.AllSuccess())

	code, err = contracts.GetCode("tests/StaticCallQuery")
	s.Require().NoError(err)
	payload = types.BuildDeployPayload(code, common.EmptyHash)

	addrQuery, receipt := s.DeployContractViaMainSmartAccount(types.BaseShardId, payload, types.GasToValue(10_000_000))
	s.Require().True(receipt.AllSuccess())

	abiQuery, err := contracts.GetAbi("tests/StaticCallQuery")
	s.Require().NoError(err)

	data := s.AbiPack(abiQuery, "checkValue", addrSource, types.NewUint256(42))
	receipt = s.SendTransactionViaSmartAccountNoCheck(types.MainSmartAccountAddress, addrQuery, execution.MainPrivateKey, data,
		types.NewFeePackFromGas(200_000), types.NewZeroValue(), nil)
	s.Require().True(receipt.AllSuccess())

	data = s.AbiPack(abiQuery, "querySyncIncrement", addrSource)
	receipt = s.SendTransactionViaSmartAccountNoCheck(types.MainSmartAccountAddress, addrQuery, execution.MainPrivateKey, data,
		types.NewFeePackFromGas(200_000), types.NewZeroValue(), nil)
	s.Require().True(receipt.AllSuccess())

	data = s.AbiPack(abiQuery, "checkValue", addrSource, types.NewUint256(43))
	receipt = s.SendTransactionViaSmartAccountNoCheck(types.MainSmartAccountAddress, addrQuery, execution.MainPrivateKey, data,
		types.NewFeePackFromGas(200_000), types.NewZeroValue(), nil)
	s.Require().True(receipt.AllSuccess())
}

func (s *SuiteRegression) TestEmptyError() {
	abi, err := contracts.GetAbi(contracts.NameTest)
	s.Require().NoError(err)

	data := s.AbiPack(abi, "returnEmptyError")
	receipt := s.SendExternalTransactionNoCheck(data, s.testAddress)
	s.Require().False(receipt.Success)
}

func TestRegression(t *testing.T) {
	t.Parallel()

	suite.Run(t, new(SuiteRegression))
}
