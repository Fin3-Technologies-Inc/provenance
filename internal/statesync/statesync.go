package statesync

import (
	"github.com/cosmos/cosmos-sdk/version"
	tmrpccore "github.com/tendermint/tendermint/rpc/core"
	tmrpc "github.com/tendermint/tendermint/rpc/jsonrpc/server"
	tmrpctypes "github.com/tendermint/tendermint/rpc/jsonrpc/types"
)

// TODO: Overhaul this statesync stuff.
//       I /think/ we will now need to use github.com/tendermint/tendermint/rpc/client/http
//       The tmrpccore stuff has been moved into their internal directory, so we can't use it.

func RegisterSyncStatus() {
	tmrpccore.Routes["sync_info"] = tmrpc.NewRPCFunc(GetSyncInfoAtBlock, "height")
}

func GetSyncInfoAtBlock(ctx *tmrpctypes.Context, height *int64) (*GetSyncInfo, error) {
	block, err := tmrpccore.Block(ctx, height)
	if err != nil {
		return nil, err
	}
	versionInfo := version.NewInfo()
	si := &GetSyncInfo{
		BlockHeight: block.Block.Header.Height,
		BlockHash:   block.Block.Header.Hash().String(),
		Version:     versionInfo.Version,
	}
	return si, nil
}

type GetSyncInfo struct {
	BlockHeight int64  `json:"block_height"`
	BlockHash   string `json:"block_hash"`
	Version     string `json:"version"`
}
