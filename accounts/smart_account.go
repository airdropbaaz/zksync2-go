package accounts

import (
	"context"
	"errors"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/zksync-sdk/zksync2-go/clients"
	"github.com/zksync-sdk/zksync2-go/contracts/erc20"
	"github.com/zksync-sdk/zksync2-go/contracts/ethtoken"
	"github.com/zksync-sdk/zksync2-go/contracts/l2bridge"
	"github.com/zksync-sdk/zksync2-go/contracts/nonceholder"
	"github.com/zksync-sdk/zksync2-go/eip712"
	zkTypes "github.com/zksync-sdk/zksync2-go/types"
	"github.com/zksync-sdk/zksync2-go/utils"
	"math/big"
)

// SmartAccount is a signer which can be configured to sign various payloads using a provided secret.
// The secret can be in any form, allowing for flexibility when working with different account implementations.
// The SmartAccount is bound to a specific address and provides the ability to define custom method for populating transactions
// and custom signing method used for signing messages, typed data, and transactions.
type SmartAccount struct {
	address            common.Address
	secret             interface{}
	client             *clients.BaseClient
	payloadSigner      PayloadSigner
	transactionBuilder TransactionBuilder

	baseToken             common.Address
	sharedL2BridgeAddress common.Address
	chainId               *big.Int
}

// NewSmartAccount creates a new SmartAccount instance.
// By default, it uses SignPayloadWithECDSA as a signer and PopulateTransactionECDSA as a builder
// and requires private key in hex format to be provided.
func NewSmartAccount(
	address common.Address,
	secret interface{},
	signer *PayloadSigner,
	builder *TransactionBuilder,
	client *clients.BaseClient) *SmartAccount {
	if signer == nil {
		signer = &SignPayloadWithECDSA
	}
	if builder == nil {
		builder = &PopulateTransactionECDSA
	}

	return &SmartAccount{
		address:            address,
		secret:             secret,
		client:             client,
		payloadSigner:      *signer,
		transactionBuilder: *builder,
	}
}

// Connect creates a new instance of SmartAccount connected to a client or
// detached from any provider if nil is provided.
func (a *SmartAccount) Connect(client *clients.BaseClient) *SmartAccount {
	return NewSmartAccount(
		a.address,
		a.secret,
		&a.payloadSigner,
		&a.transactionBuilder,
		client)
}

// Address returns the address of the associated account.
func (a *SmartAccount) Address() common.Address {
	return a.address
}

// Balance returns the balance of the specified token that can be either ETH or any ERC20 token.
// The block number can be nil, in which case the balance is taken from the latest known block.
func (a *SmartAccount) Balance(ctx context.Context, token common.Address, at *big.Int) (*big.Int, error) {
	err := a.cacheData(ctx)
	if err != nil {
		return nil, err
	}
	if token == utils.LegacyEthAddress || token == a.baseToken {
		return a.client.BalanceAt(ensureContext(ctx), a.Address(), at)
	}
	erc20Token, err := erc20.NewIERC20(token, a.client)
	if err != nil {
		return nil, err
	}
	return erc20Token.BalanceOf(&bind.CallOpts{
		From:        a.Address(),
		BlockNumber: at,
		Context:     ensureContext(ctx),
	}, a.Address())
}

// AllBalances returns all balances for confirmed tokens given by an associated account.
func (a *SmartAccount) AllBalances(ctx context.Context) (map[common.Address]*big.Int, error) {
	return a.client.AllAccountBalances(ensureContext(ctx), a.Address())
}

// Nonce returns the account nonce of the associated account.
// The block number can be nil, in which case the nonce is taken from the latest known block.
func (a *SmartAccount) Nonce(ctx context.Context, blockNumber *big.Int) (uint64, error) {
	return a.client.NonceAt(ctx, a.Address(), blockNumber)
}

// DeploymentNonce returns the deployment nonce of the account.
func (a *SmartAccount) DeploymentNonce(opts *CallOpts) (*big.Int, error) {
	callOpts := ensureCallOpts(opts).ToCallOpts(a.Address())
	nonceHolder, err := nonceholder.NewINonceHolder(utils.NonceHolderAddress, a.client)
	if err != nil {
		return nil, err
	}
	return nonceHolder.GetDeploymentNonce(callOpts, a.Address())
}

// PopulateTransaction populates the transaction tx using the provided TransactionBuilder function.
// If tx.From is not set, it sets the value from the Address method which can
// be utilized in the TransactionBuilder function.
func (a *SmartAccount) PopulateTransaction(ctx context.Context, tx *zkTypes.Transaction712) error {
	if tx.From == nil {
		from := a.Address()
		tx.From = &from
	}
	return a.transactionBuilder(ensureContext(ctx), tx, a.secret, a.client)
}

// SignTransaction returns a signed transaction that is ready to be broadcast to
// the network. The PopulateTransaction method is called first to ensure that all
// necessary properties for the transaction to be valid have been populated.
func (a *SmartAccount) SignTransaction(ctx context.Context, tx *zkTypes.Transaction712) ([]byte, error) {
	err := a.cacheData(ensureContext(ctx))
	if err != nil {
		return nil, err
	}

	err = a.PopulateTransaction(ensureContext(ctx), tx)
	if err != nil {
		return nil, err
	}

	domain := eip712.ZkSyncEraEIP712Domain(a.chainId.Int64())

	eip712Msg, err := tx.EIP712Message()
	if err != nil {
		return nil, err
	}

	signature, err := a.SignTypedData(ctx, apitypes.TypedData{
		Types: apitypes.Types{
			tx.EIP712Type():     tx.EIP712Types(),
			domain.EIP712Type(): domain.EIP712Types(),
		},
		PrimaryType: tx.EIP712Type(),
		Domain:      domain.EIP712Domain(),
		Message:     eip712Msg,
	})
	if err != nil {
		return nil, err
	}
	return tx.RLPValues(signature)
}

// SendTransaction injects a transaction into the pending pool for execution.
// The SignTransaction is called first to ensure transaction is properly signed.
func (a *SmartAccount) SendTransaction(ctx context.Context, tx *zkTypes.Transaction712) (common.Hash, error) {
	rawTx, err := a.SignTransaction(ensureContext(ctx), tx)
	if err != nil {
		return common.Hash{}, err
	}
	return a.client.SendRawTransaction(context.Background(), rawTx)
}

// SignMessage signs a message using the provided PayloadSigner function.
func (a *SmartAccount) SignMessage(ctx context.Context, message []byte) ([]byte, error) {
	return a.payloadSigner(ensureContext(ctx), accounts.TextHash(message), a.secret, a.client)
}

// SignTypedData signs a typed data using the provided PayloadSigner function.
func (a *SmartAccount) SignTypedData(ctx context.Context, typedData apitypes.TypedData) ([]byte, error) {
	hash, _, err := apitypes.TypedDataAndHash(typedData)
	if err != nil {
		return nil, err
	}
	signature, err := a.payloadSigner(ctx, hash, a.secret, a.client)
	if err != nil {
		return nil, err
	}
	return signature, nil
}

// Withdraw initiates the withdrawal process which withdraws ETH or any ERC20
// token from the associated account on L2 network to the target account on L1
// network.
func (a *SmartAccount) Withdraw(auth *TransactOpts, tx WithdrawalTransaction) (common.Hash, error) {
	from := a.Address()

	opts := ensureTransactOpts(auth)
	a.insertGasPrice(opts)
	err := a.cacheData(opts.Context)
	if err != nil {
		return common.Hash{}, err
	}

	if tx.Token == utils.LegacyEthAddress {
		tx.Token = utils.EthAddressInContracts
		if opts.Value != nil && opts.Value != tx.Amount {
			return common.Hash{}, errors.New("the tx.value is not equal to the value withdrawn")
		} else {
			opts.Value = tx.Amount
		}

		abi, abiErr := ethtoken.IEthTokenMetaData.GetAbi()
		if abiErr != nil {
			return common.Hash{}, abiErr
		}

		data, packErr := abi.Pack("withdraw", tx.To)
		if packErr != nil {
			return common.Hash{}, packErr
		}

		return a.SendTransaction(opts.Context, &zkTypes.Transaction712{
			Nonce:     opts.Nonce,
			GasTipCap: opts.GasTipCap,
			GasFeeCap: opts.GasFeeCap,
			Gas:       new(big.Int).SetUint64(opts.GasLimit),
			To:        &utils.L2BaseTokenAddress,
			Value:     opts.Value,
			Data:      data,
			From:      &from,
			ChainID:   a.chainId,
			Meta: &zkTypes.Eip712Meta{
				GasPerPubdata:   utils.NewBig(utils.DefaultGasPerPubdataLimit.Int64()),
				PaymasterParams: tx.PaymasterParams,
			},
		})
	}

	if tx.BridgeAddress == nil {
		tx.BridgeAddress = &a.sharedL2BridgeAddress
	}

	abi, abiErr := l2bridge.IL2BridgeMetaData.GetAbi()
	if abiErr != nil {
		return common.Hash{}, abiErr
	}

	data, abiPack := abi.Pack("withdraw", tx.To, tx.Token, tx.Amount)
	if abiPack != nil {
		return common.Hash{}, abiPack
	}

	return a.SendTransaction(opts.Context, &zkTypes.Transaction712{
		Nonce:     opts.Nonce,
		GasTipCap: opts.GasTipCap,
		GasFeeCap: opts.GasFeeCap,
		Gas:       new(big.Int).SetUint64(opts.GasLimit),
		To:        tx.BridgeAddress,
		Value:     opts.Value,
		Data:      data,
		ChainID:   a.chainId,
		From:      &from,
		Meta: &zkTypes.Eip712Meta{
			GasPerPubdata:   utils.NewBig(utils.DefaultGasPerPubdataLimit.Int64()),
			PaymasterParams: tx.PaymasterParams,
		},
	})
}

// Transfer moves the ETH or any ERC20 token from the associated account to the target account.
func (a *SmartAccount) Transfer(auth *TransactOpts, tx TransferTransaction) (common.Hash, error) {
	from := a.Address()

	opts := ensureTransactOpts(auth)
	a.insertGasPrice(opts)
	err := a.cacheData(opts.Context)
	if err != nil {
		return common.Hash{}, err
	}

	if isBaseToken, baseTokenErr := a.isBaseToken(opts.Context, tx.Token); tx.Token == utils.LegacyEthAddress || isBaseToken {
		return a.SendTransaction(opts.Context, &zkTypes.Transaction712{
			Nonce:     opts.Nonce,
			GasTipCap: opts.GasTipCap,
			GasFeeCap: opts.GasFeeCap,
			Gas:       new(big.Int).SetUint64(opts.GasLimit),
			To:        &tx.To,
			Value:     tx.Amount,
			ChainID:   a.chainId,
			From:      &from,
			Meta: &zkTypes.Eip712Meta{
				GasPerPubdata:   utils.NewBig(utils.DefaultGasPerPubdataLimit.Int64()),
				PaymasterParams: tx.PaymasterParams,
			},
		})
	} else if baseTokenErr != nil {
		return common.Hash{}, baseTokenErr
	}

	abi, err := erc20.IERC20MetaData.GetAbi()
	if err != nil {
		return common.Hash{}, err
	}

	data, err := abi.Pack("transfer", tx.To, tx.Amount)
	if err != nil {
		return common.Hash{}, err
	}

	return a.SendTransaction(opts.Context, &zkTypes.Transaction712{
		Nonce:     opts.Nonce,
		GasTipCap: opts.GasTipCap,
		GasFeeCap: opts.GasFeeCap,
		Gas:       new(big.Int).SetUint64(opts.GasLimit),
		To:        &tx.Token,
		Value:     big.NewInt(0),
		Data:      data,
		ChainID:   a.chainId,
		From:      &from,
		Meta: &zkTypes.Eip712Meta{
			GasPerPubdata:   utils.NewBig(utils.DefaultGasPerPubdataLimit.Int64()),
			PaymasterParams: tx.PaymasterParams,
		},
	})
}

func (a *SmartAccount) cacheData(ctx context.Context) error {
	var err error
	if a.chainId == nil {
		a.chainId, err = a.client.ChainID(ensureContext(ctx))
		if err != nil {
			return err
		}
	}

	if a.baseToken == (common.Address{}) {
		a.baseToken, err = a.client.BaseTokenContractAddress(ctx)
		if err != nil {
			return err
		}
	}

	if a.sharedL2BridgeAddress == (common.Address{}) {
		bridges, bridgeErr := a.client.BridgeContracts(ctx)
		if err != nil {
			return bridgeErr
		}
		a.sharedL2BridgeAddress = bridges.L2SharedBridge
	}
	return nil
}

func (a *SmartAccount) insertGasPrice(opts *TransactOpts) {
	if opts.GasPrice != nil {
		opts.GasFeeCap = opts.GasPrice
		opts.GasTipCap = nil
		opts.GasPrice = nil
	}
}

func (a *SmartAccount) isBaseToken(ctx context.Context, token common.Address) (bool, error) {
	return a.client.IsBaseToken(ensureContext(ctx), token)
}

// NewECDSASmartAccount creates a SmartAccount instance that uses single ECDSA key for signing payload.
func NewECDSASmartAccount(address common.Address, privateKey string, client *clients.BaseClient) *SmartAccount {
	return NewSmartAccount(address, privateKey, &SignPayloadWithECDSA, &PopulateTransactionECDSA, client)
}

// NewMultisigECDSASmartAccount creates a SmartAccount instance that uses multiple ECDSA keys for signing payloads.
// The signature is generated by concatenating signatures created by signing with each key individually.
func NewMultisigECDSASmartAccount(address common.Address, privateKeys []string, client *clients.BaseClient) *SmartAccount {
	return NewSmartAccount(address, privateKeys, &SignPayloadWithMultipleECDSA, &PopulateTransactionMultipleECDSA, client)
}
