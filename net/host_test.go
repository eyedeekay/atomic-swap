// Copyright 2023 The AthanorLabs/atomic-swap Authors
// SPDX-License-Identifier: LGPL-3.0-only

package net

import (
	"context"
	"path"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"

	"github.com/athanorlabs/atomic-swap/common"
	"github.com/athanorlabs/atomic-swap/common/types"
	"github.com/athanorlabs/atomic-swap/net/message"
)

func init() {
	logging.SetLogLevel("net", "debug")
	logging.SetLogLevel("p2pnet", "debug")
}

var (
	testID        = types.Hash{99}
	mockEthTXHash = ethcommon.Hash{33}
)

type mockMakerHandler struct {
	t  *testing.T
	id types.Hash
}

func (*mockMakerHandler) GetOffers() []*types.Offer {
	return []*types.Offer{}
}

func (h *mockMakerHandler) HandleInitiateMessage(
	_ peer.ID,
	msg *message.SendKeysMessage,
) (s SwapState, err error) {
	if (h.id != types.Hash{}) {
		return &mockSwapState{h.id}, nil
	}
	return &mockSwapState{}, nil
}

type mockRelayHandler struct {
	t *testing.T
}

func (*mockRelayHandler) GetRelayerAddressHash() (types.Hash, error) {
	return types.Hash{99}, nil
}

func (*mockRelayHandler) HasOngoingSwapAsTaker(_ peer.ID) error {
	return nil
}

func (*mockRelayHandler) HandleRelayClaimRequest(_ peer.ID, _ *RelayClaimRequest) (*RelayClaimResponse, error) {
	return &RelayClaimResponse{
		TxHash: mockEthTXHash,
	}, nil
}

type mockSwapState struct {
	offerID types.Hash
}

func (*mockSwapState) NotifyStreamClosed() {}

func (s *mockSwapState) OfferID() types.Hash {
	if (s.offerID != types.Hash{}) {
		return s.offerID
	}

	return testID
}

func (*mockSwapState) HandleProtocolMessage(_ Message) error {
	return nil
}

func (*mockSwapState) Exit() error {
	return nil
}

func basicTestConfig(t *testing.T) *Config {
	// t.TempDir() is unique on every call. Don't reuse this config with multiple hosts.
	tmpDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
	})

	return &Config{
		Ctx:       ctx,
		Env:       common.Development,
		DataDir:   tmpDir,
		Port:      0, // OS randomized libp2p port
		KeyFile:   path.Join(tmpDir, "node.key"),
		Bootnodes: nil,
		ListenIP:  "127.0.0.1",
		IsRelayer: false,
	}
}

func newHost(t *testing.T, cfg *Config) *Host {
	h, err := NewHost(cfg)
	require.NoError(t, err)
	h.SetHandlers(&mockMakerHandler{t: t}, &mockRelayHandler{t: t})
	t.Cleanup(func() {
		err = h.Stop()
		require.NoError(t, err)
	})
	return h
}
