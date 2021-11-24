package alice

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/noot/atomic-swap/monero"
	"github.com/noot/atomic-swap/net"
	"github.com/noot/atomic-swap/swap-contract"

	ethcommon "github.com/ethereum/go-ethereum/common"
)

var nextID uint64 = 0

var (
	errMissingKeys         = errors.New("did not receive Bob's public spend or private view key")
	errMissingSpendKeyHash = errors.New("did not receive Bob's spend key hash")
	errMissingAddress      = errors.New("did not receive Bob's address")
)

// swapState is an instance of a swap. it holds the info needed for the swap,
// and its current state.
type swapState struct {
	*alice
	ctx    context.Context
	cancel context.CancelFunc

	id uint64
	// amount of ETH we are providing this swap, and the amount of XMR we should receive.
	providesAmount, desiredAmount uint64

	// our keys for this session
	privkeys *monero.PrivateKeyPair
	pubkeys  *monero.PublicKeyPair

	// Bob's keys for this session
	bobPublicSpendKey *monero.PublicKey
	bobPrivateViewKey *monero.PrivateViewKey
	bobClaimHash      [32]byte
	bobAddress        ethcommon.Address

	// swap contract and timeouts in it; set once contract is deployed
	contract *swap.Swap
	t0, t1   time.Time

	// next expected network message
	nextExpectedMessage net.Message // TODO: change to type?

	// channels
	xmrLockedCh chan struct{}
	claimedCh   chan struct{}

	// set to true upon creating of the XMR wallet
	success bool
}

func newSwapState(a *alice, providesAmount, desiredAmount uint64) *swapState {
	ctx, cancel := context.WithCancel(a.ctx)

	s := &swapState{
		ctx:                 ctx,
		cancel:              cancel,
		alice:               a,
		id:                  nextID,
		providesAmount:      providesAmount,
		desiredAmount:       desiredAmount,
		nextExpectedMessage: &net.SendKeysMessage{}, // should this be &net.InitiateMessage{}?
		xmrLockedCh:         make(chan struct{}),
		claimedCh:           make(chan struct{}),
	}

	nextID++
	return s
}

func (s *swapState) SendKeysMessage() (*net.SendKeysMessage, error) {
	kp, err := s.generateKeys()
	if err != nil {
		return nil, err
	}

	sh := s.privkeys.SpendKey().Hash()

	return &net.SendKeysMessage{
		PublicSpendKey: kp.SpendKey().Hex(),
		PrivateViewKey: s.privkeys.ViewKey().Hex(),
		SpendKeyHash:   hex.EncodeToString(sh[:]),
	}, nil
}

// ProtocolComplete is called by the network when the protocol stream closes.
// If it closes prematurely, we need to perform recovery.
func (s *swapState) ProtocolComplete() {
	defer func() {
		// stop all running goroutines
		s.cancel()
		s.alice.swapState = nil
	}()

	if s.success {
		return
	}

	switch s.nextExpectedMessage.(type) {
	case *net.SendKeysMessage:
		// we are fine, as we only just initiated the protocol.
	case *net.NotifyXMRLock:
		// we already deployed the contract, so we should call Refund().
		if err := s.tryRefund(); err != nil {
			log.Errorf("failed to refund: err=%s", err)
			return
		}
	case *net.NotifyClaimed:
		// the XMR has been locked, but the ETH hasn't been claimed.
		// we should also refund in this case.
		if err := s.tryRefund(); err != nil {
			log.Errorf("failed to refund: err=%s", err)
			return
		}
	default:
		log.Errorf("unexpected nextExpectedMessage in ProtocolComplete: type=%T", s.nextExpectedMessage)
	}
}

func (s *swapState) tryRefund() error {
	untilT0 := time.Until(s.t0)
	untilT1 := time.Until(s.t1)

	if untilT0 > 0 && untilT1 < 0 {
		// we've passed t0 but aren't past t1 yet, so we need to wait until t1
		log.Infof("waiting until time %s to refund", s.t1)
		<-time.After(untilT1)
	}

	_, err := s.refund()
	return err
}

// HandleProtocolMessage is called by the network to handle an incoming message.
// If the message received is not the expected type for the point in the protocol we're at,
// this function will return an error.
func (s *swapState) HandleProtocolMessage(msg net.Message) (net.Message, bool, error) {
	if err := s.checkMessageType(msg); err != nil {
		return nil, true, err
	}

	switch msg := msg.(type) {
	case *net.SendKeysMessage:
		resp, err := s.handleSendKeysMessage(msg)
		if err != nil {
			return nil, true, err
		}

		return resp, false, nil
	case *net.NotifyXMRLock:
		if msg.Address == "" {
			return nil, true, errors.New("got empty address for locked XMR")
		}

		// check that XMR was locked in expected account, and confirm amount
		vk := monero.SumPrivateViewKeys(s.bobPrivateViewKey, s.privkeys.ViewKey())
		sk := monero.SumPublicKeys(s.bobPublicSpendKey, s.pubkeys.SpendKey())
		kp := monero.NewPublicKeyPair(sk, vk.Public())

		if msg.Address != string(kp.Address(s.alice.env)) {
			return nil, true, fmt.Errorf("address received in message does not match expected address")
		}

		t := time.Now().Format("2006-Jan-2-15:04:05")
		walletName := fmt.Sprintf("alice-viewonly-wallet-%s", t)
		if err := s.alice.client.GenerateViewOnlyWalletFromKeys(vk, kp.Address(s.alice.env), walletName, ""); err != nil {
			return nil, true, fmt.Errorf("failed to generate view-only wallet to verify locked XMR: %w", err)
		}

		if err := s.alice.client.Refresh(); err != nil {
			return nil, true, err
		}

		balance, err := s.alice.client.GetBalance(0)
		if err != nil {
			return nil, true, err
		}

		log.Debugf("checking locked wallet, address=%s balance=%v", kp.Address(s.alice.env), balance.Balance)
		log.Debug("public spend keys for lock account: ", kp.SpendKey().Hex())
		log.Debug("public view keys for lock account: ", kp.ViewKey().Hex())

		// TODO: also check that the balance isn't unlocked only after an unreasonable amount of blocks
		if balance.Balance < float64(s.desiredAmount) {
			return nil, true, fmt.Errorf("locked XMR amount is less than expected: got %v, expected %v", balance.Balance, float64(s.desiredAmount))
		}

		if err := s.alice.client.CloseWallet(); err != nil {
			return nil, true, fmt.Errorf("failed to close wallet: %w", err)
		}

		s.nextExpectedMessage = &net.NotifyClaimed{}
		close(s.xmrLockedCh)

		if err := s.ready(); err != nil {
			return nil, true, fmt.Errorf("failed to call Ready: %w", err)
		}

		log.Debug("set swap.IsReady == true")

		go func() {
			until := time.Until(s.t1)

			select {
			case <-s.ctx.Done():
				return
			case <-time.After(until):
				// Bob hasn't claimed, and we're after t_1. let's call Refund
				txhash, err := s.refund()
				if err != nil {
					log.Errorf("failed to refund: err=%s", err)
					return
				}

				log.Infof("got our ETH back: tx hash=%s", txhash)

				// send NotifyRefund msg
				if err = s.net.SendSwapMessage(&net.NotifyRefund{
					TxHash: txhash,
				}); err != nil {
					log.Errorf("failed to send refund message: err=%s", err)
				}
			case <-s.claimedCh:
				return
			}
		}()

		out := &net.NotifyReady{}
		return out, false, nil
	case *net.NotifyClaimed:
		address, err := s.handleNotifyClaimed(msg.TxHash)
		if err != nil {
			log.Error("failed to create monero address: err=", err)
			return nil, true, err
		}

		close(s.claimedCh)

		log.Info("successfully created monero wallet from our secrets: address=", address)
		return nil, true, nil
	default:
		return nil, false, errors.New("unexpected message type")
	}
}

func (s *swapState) verifyBobKeys(msg *net.SendKeysMessage) error { // TODO: this is the same for Alice and Bob, move to common somewhere
	hb, err := hex.DecodeString(msg.SpendKeyHash)
	if err != nil {
		return err
	}

	if len(hb) != 32 {
		return errors.New("invalid spend key hash")
	}

	copy(s.bobClaimHash[:], hb)

	// check that spend keyhash can be derived to view key
	dvk, err := monero.NewPrivateViewKeyFromHash(msg.SpendKeyHash)
	if err != nil {
		return fmt.Errorf("failed to derive view key from spend key hash: %w", err)
	}

	vk, err := monero.NewPrivateViewKeyFromHex(msg.PrivateViewKey)
	if err != nil {
		return fmt.Errorf("failed to generate Bob's private view keys: %w", err)
	}

	if vk.Hex() != dvk.Hex() {
		return fmt.Errorf("derived view key does not match message's view key: derived=%s received=%s", dvk.Hex(), vk.Hex())
	}

	kp, err := monero.NewPublicKeyPairFromHex(msg.PublicSpendKey, vk.Public().Hex())
	if err != nil {
		return fmt.Errorf("failed to generate Alice's public keys: %w", err)
	}

	// check that wallet can be created using Bob's private view key and public spend key
	t := time.Now().Format("2006-Jan-2-15:04:05")
	walletName := fmt.Sprintf("bob-viewonly-wallet-%s", t)
	if err = s.alice.client.GenerateViewOnlyWalletFromKeys(vk, kp.Address(s.alice.env), walletName, ""); err != nil {
		return fmt.Errorf("failed to generate view-only wallet to verify Bob's keys: %w", err)
	}

	// can close it right after, as we were just checking that they correspond
	if err = s.alice.client.CloseWallet(); err != nil {
		return fmt.Errorf("failed to close wallet: %w", err)
	}

	return nil
}

func (s *swapState) handleSendKeysMessage(msg *net.SendKeysMessage) (net.Message, error) {
	if msg.PublicSpendKey == "" || msg.PrivateViewKey == "" {
		return nil, errMissingKeys
	}

	if msg.SpendKeyHash == "" {
		return nil, errMissingSpendKeyHash
	}

	if msg.EthAddress == "" {
		return nil, errMissingAddress
	}

	if err := s.verifyBobKeys(msg); err != nil {
		return nil, err
	}

	vk, err := monero.NewPrivateViewKeyFromHex(msg.PrivateViewKey)
	if err != nil {
		return nil, fmt.Errorf("failed to generate Bob's private view keys: %w", err)
	}

	s.bobAddress = ethcommon.HexToAddress(msg.EthAddress)

	log.Debugf("got Bob's keys and address: address=%s", s.bobAddress)
	s.nextExpectedMessage = &net.NotifyXMRLock{}

	sk, err := monero.NewPublicKeyFromHex(msg.PublicSpendKey)
	if err != nil {
		return nil, fmt.Errorf("failed to generate Bob's public spend key: %w", err)
	}
	s.setBobKeys(sk, vk)
	address, err := s.deployAndLockETH(s.providesAmount)
	if err != nil {
		return nil, fmt.Errorf("failed to deploy contract: %w", err)
	}

	log.Info("deployed Swap contract, waiting for XMR to be locked: contract address=", address)

	// set t0 and t1
	st0, err := s.contract.Timeout0(s.alice.callOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to get timeout0 from contract: err=%w", err)
	}

	s.t0 = time.Unix(st0.Int64(), 0)

	st1, err := s.contract.Timeout1(s.alice.callOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to get timeout1 from contract: err=%w", err)
	}

	s.t1 = time.Unix(st1.Int64(), 0)

	// start goroutine to check that Bob locks before t_0
	go func() {
		const timeoutBuffer = time.Minute * 5
		until := time.Until(s.t0)

		select {
		case <-s.ctx.Done():
			return
		case <-time.After(until - timeoutBuffer):
			// Bob hasn't locked yet, let's call refund
			txhash, err := s.refund()
			if err != nil {
				log.Errorf("failed to refund: err=%s", err)
				return
			}

			log.Infof("got our ETH back: tx hash=%s", txhash)

			// send NotifyRefund msg
			if err := s.net.SendSwapMessage(&net.NotifyRefund{
				TxHash: txhash,
			}); err != nil {
				log.Errorf("failed to send refund message: err=%s", err)
			}
		case <-s.xmrLockedCh:
			return
		}

	}()

	out := &net.NotifyContractDeployed{
		Address: address.String(),
	}

	return out, nil
}

func (s *swapState) checkMessageType(msg net.Message) error {
	if msg.Type() != s.nextExpectedMessage.Type() {
		return errors.New("received unexpected message")
	}

	return nil
}