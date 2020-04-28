package wasmvm

import (
	encjson "encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ava-labs/gecko/utils/crypto"
	"github.com/ava-labs/gecko/utils/formatting"
	"github.com/ava-labs/gecko/utils/json"

	"github.com/ava-labs/gecko/ids"
	"github.com/ava-labs/gecko/snow/engine/common"
)

var errBagByteArgs = errors.New("expected 'byteArgs' to be JSON or base58 formatted bytes but was neither")

// CreateStaticHandlers returns a map where:
// Keys: The path extension for this VM's static API
// Values: The handler for that static API
// We return nil because this VM has no static API
func (vm *VM) CreateStaticHandlers() map[string]*common.HTTPHandler { return nil }

// CreateHandlers returns a map where:
// * keys are API endpoint extensions
// * values are API handlers
// See API documentation for more information
func (vm *VM) CreateHandlers() map[string]*common.HTTPHandler {
	handler := vm.SnowmanVM.NewHandler("wasm", &Service{vm: vm})
	return map[string]*common.HTTPHandler{"": handler}
}

// Service is the API service
type Service struct {
	vm *VM
}

// NewKeyResponse ...
type NewKeyResponse struct {
	// A new private key
	Key formatting.CB58 `json:"privateKey"`
}

// NewKey returns a new private key
func (s *Service) NewKey(_ *http.Request, args *struct{}, response *NewKeyResponse) error {
	key, err := keyFactory.NewPrivateKey()
	if err != nil {
		return fmt.Errorf("couldn't create new private key: %v", err)
	}
	response.Key = formatting.CB58{Bytes: key.Bytes()}
	return nil
}

// ArgAPI is the API repr of a function argument
type ArgAPI struct {
	Type  string      `json:"type"`
	Value interface{} `json:"value"`
}

// Return argument as its go type
func (arg *ArgAPI) toFnArg() (interface{}, error) {
	switch strings.ToLower(arg.Type) {
	case "int32":
		if valInt32, ok := arg.Value.(int32); ok {
			return valInt32, nil
		}
		if valInt64, ok := arg.Value.(int64); ok {
			return int32(valInt64), nil
		}
		if valFloat32, ok := arg.Value.(float32); ok {
			return int32(valFloat32), nil
		}
		if valFloat64, ok := arg.Value.(float64); ok {
			return int32(valFloat64), nil
		}
		return nil, fmt.Errorf("value '%v' is not convertible to int32", arg.Value)
	case "int64":
		if valInt32, ok := arg.Value.(int32); ok {
			return int64(valInt32), nil
		}
		if valInt64, ok := arg.Value.(int64); ok {
			return valInt64, nil
		}
		if valFloat32, ok := arg.Value.(float32); ok {
			return int64(valFloat32), nil
		}
		if valFloat64, ok := arg.Value.(float64); ok {
			return int64(valFloat64), nil
		}
		return nil, fmt.Errorf("value '%v' is not convertible to int64", arg.Value)
	default:
		return nil, errors.New("arg type must be one of: int32, int64")
	}
}

// InvokeArgs ...
type InvokeArgs struct {
	// Contract to invoke
	ContractID ids.ID `json:"contractID"`
	// Function in contract to invoke
	Function string `json:"function"`
	// Private Key signing the invocation tx
	// This key's address is the "sender" of the tx
	// Must be byte repr. of a SECP256K1R private key
	SenderKey formatting.CB58 `json:"senderKey"`
	// Sender's next unused nonce
	SenderNonce json.Uint64 `json:"senderNonce"`
	// Integer arguments to the function
	Args []ArgAPI `json:"args"`
	// Byte arguments to the function
	ByteArgs interface{} `json:"byteArgs"`
}

func (args *InvokeArgs) validate() error {
	switch {
	case len(args.SenderKey.Bytes) == 0:
		return errors.New("argument 'senderKey' not provided")
	case uint64(args.SenderNonce) == 0:
		return errors.New("'senderNonce' must be at least 1")
	case args.ContractID.Equals(ids.Empty):
		return errors.New("contractID not specified")
	case args.Function == "":
		return errors.New("function not specified")
	}
	return nil
}

func (args *InvokeArgs) getByteArgs() ([]byte, error) {
	if args.ByteArgs == nil {
		return []byte{}, nil
	}
	// If byteArgs are JSON, marshal them to bytes
	// Only top-level array or object is accepted as valid JSON
	switch args.ByteArgs.(type) {
	case []interface{}, map[string]interface{}:
		if bytes, err := encjson.Marshal(args.ByteArgs); err == nil {
			return bytes, nil
		}
		return nil, errBagByteArgs
	}

	// Otherwise, try to parse them as base 58 string
	asStr, ok := args.ByteArgs.(string)
	if !ok {
		return nil, fmt.Errorf("expected 'byteArgs' to be JSON or base58 formatted bytes but was neither")
	}
	formatter := formatting.CB58{}
	if err := formatter.FromString(asStr); err != nil {
		return nil, fmt.Errorf("expected 'byteArgs' to be JSON or base58 formatted bytes but was neither")
	}
	return formatter.Bytes, nil
}

// InvokeResponse ...
type InvokeResponse struct {
	TxID ids.ID `json:"txID"`
}

// Invoke ...
func (s *Service) Invoke(_ *http.Request, args *InvokeArgs, response *InvokeResponse) error {
	s.vm.Ctx.Log.Debug("in invoke")
	if err := args.validate(); err != nil {
		return fmt.Errorf("arguments failed validation: %v", err)
	}

	fnArgs := make([]interface{}, len(args.Args))
	var err error
	for i, arg := range args.Args {
		fnArgs[i], err = arg.toFnArg()
		if err != nil {
			return fmt.Errorf("couldn't parse arg '%+v': %s", arg, err)
		}
	}

	// Parse byteArgs
	byteArgs, err := args.getByteArgs()
	if err != nil {
		return fmt.Errorf("couldn't parse 'byteArgs': %v", err)
	}

	senderKeyIntf, err := keyFactory.ToPrivateKey(args.SenderKey.Bytes)
	if err != nil {
		return fmt.Errorf("couldn't parse 'privateKey' to a SECP256K1R private key: %v", err)
	}
	senderKey, ok := senderKeyIntf.(*crypto.PrivateKeySECP256K1R)
	if !ok {
		return fmt.Errorf("couldn't parse 'privateKey' to a SECP256K1R private key: %v", err)
	}

	tx, err := s.vm.newInvokeTx(args.ContractID, args.Function, fnArgs, byteArgs, uint64(args.SenderNonce), senderKey)
	if err != nil {
		return fmt.Errorf("couldn't create tx: %s", err)
	}

	// Add tx to mempool
	s.vm.mempool = append(s.vm.mempool, tx)
	s.vm.NotifyBlockReady()

	response.TxID = tx.ID()
	return nil
}

// CreateContractArgs ...
type CreateContractArgs struct {
	// The byte representation of the contract.
	// Must be a valid wasm file.
	Contract formatting.CB58 `json:"contract"`

	// Byte repr. of the private key of the sender of this tx
	// Should be a SECP256K1R private key
	SenderKey formatting.CB58 `json:"senderKey"`

	// Next unused nonce of the sender
	SenderNonce json.Uint64 `json:"senderNonce"`
}

// CreateContract creates a new contract
// The contract's ID is the ID of the tx that creates it, which is returned by this method
func (s *Service) CreateContract(_ *http.Request, args *CreateContractArgs, response *ids.ID) error {
	s.vm.Ctx.Log.Debug("in createContract")

	// validation
	if len(args.SenderKey.Bytes) == 0 {
		return errors.New("argument 'senderKey' not given")
	}
	if len(args.Contract.Bytes) == 0 {
		return errors.New("argument 'contract' not given")
	}
	if uint64(args.SenderNonce) == 0 {
		return errors.New("argument 'senderNonce' must be at least 1")
	}

	// Parse key
	senderKeyIntf, err := keyFactory.ToPrivateKey(args.SenderKey.Bytes)
	if err != nil {
		return fmt.Errorf("couldn't parse 'senderKey' to a SECP256K1R private key: %v", err)
	}
	senderKey, ok := senderKeyIntf.(*crypto.PrivateKeySECP256K1R)
	if !ok {
		return fmt.Errorf("couldn't parse 'senderKey' to a SECP256K1R private key: %v", err)
	}

	// Create tx
	tx, err := s.vm.newCreateContractTx(args.Contract.Bytes, uint64(args.SenderNonce), senderKey)
	if err != nil {
		return fmt.Errorf("couldn't create tx: %v", err)
	}

	// Add tx to mempool
	s.vm.mempool = append(s.vm.mempool, tx)
	s.vm.NotifyBlockReady()

	*response = tx.ID()
	return nil

}

// GetTxArgs ...
type GetTxArgs struct {
	ID ids.ID `json:"id"`
}

// GetTxResponse ...
type GetTxResponse struct {
	Tx *txReturnValue `json:"receipt"`
}

// GetTx returns a tx by its ID
func (s *Service) GetTx(_ *http.Request, args *GetTxArgs, response *GetTxResponse) error {
	tx, err := s.vm.getTx(s.vm.DB, args.ID)
	if err != nil {
		return fmt.Errorf("couldn't find tx with ID %s", args.ID)
	}
	response.Tx = tx
	return nil
}
