package contract

import (
	"bytes"
	"encoding/binary"
	"log"
	"math/big"
	"sync/atomic"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
)

// ── ContractConfig ────────────────────────────────────────────────────────────
// SupportedTransactions[i] MUST match TransactionTypeUrls[i] exactly —
// any mismatch causes silent misrouting per the Canopy plugin spec.

var ContractConfig = &PluginConfig{
	Name:    "go_plugin_contract",
	Id:      1,
	Version: 1,
	SupportedTransactions: []string{
		"send",
		"create_market",
		"submit_prediction",
		"resolve_market",
		"claim_winnings",
	},
	TransactionTypeUrls: []string{
		"type.googleapis.com/types.MessageSend",
		"type.googleapis.com/types.MessageCreateMarket",
		"type.googleapis.com/types.MessageSubmitPrediction",
		"type.googleapis.com/types.MessageResolveMarket",
		"type.googleapis.com/types.MessageClaimWinnings",
	},
	EventTypeUrls: nil,
}

// ── Protocol constants ────────────────────────────────────────────────────────

// MinMarketDuration is the minimum number of blocks between market creation
// and resolution. Prevents flash markets designed to exploit timing or
// frontrun the resolver.
const MinMarketDuration = 100

// MaxMarketDuration is the maximum number of blocks between market creation
// and resolution. Prevents bonds from being locked indefinitely in
// markets that can never realistically resolve.
// FIX (Security Issue 5): Added upper bound — previously unbounded.
const MaxMarketDuration = 1_000_000

// TreasuryFeeBps is the protocol treasury cut taken from the losing pool
// before paying out winners, in basis points. 200 = 2%.
// This mirrors Polymarket's revenue model.
const TreasuryFeeBps = 200

// TreasuryAddress is the protocol treasury account that receives the losing-pool
// cut on every winning claim.
// TODO: Replace with your real multisig / governance-controlled address before mainnet.
// Currently set to the ASCII bytes of "PRAXIS_TREASURY_ADDR" as a placeholder.
var TreasuryAddress = []byte{
	0x50, 0x52, 0x41, 0x58, 0x49, 0x53, 0x5f, 0x54,
	0x52, 0x45, 0x41, 0x53, 0x55, 0x52, 0x59, 0x5f,
	0x41, 0x44, 0x44, 0x52,
} // "PRAXIS_TREASURY_ADDR" — 20 bytes

// ── QueryId counter ───────────────────────────────────────────────────────────
// Uses a monotonic atomic counter instead of math/rand.Uint64().
// rand collisions within a batch silently misroute state reads.
// An atomic counter guarantees uniqueness within a process lifetime.

var queryCounter uint64

func nextQueryId() uint64 {
	return atomic.AddUint64(&queryCounter, 1)
}

// ── Overflow-safe arithmetic helpers ─────────────────────────────────────────
// FIX (Security Issue 4): uint64 multiplication in payout math can silently
// overflow, producing a tiny payout for the winner. All proportional
// calculations now go through math/big for the intermediate multiply.

// mulDiv computes (a * b) / c using big.Int for the intermediate product,
// preventing uint64 overflow. Returns 0 if c is 0.
func mulDiv(a, b, c uint64) uint64 {
	if c == 0 {
		return 0
	}
	num := new(big.Int).Mul(
		new(big.Int).SetUint64(a),
		new(big.Int).SetUint64(b),
	)
	result := new(big.Int).Div(num, new(big.Int).SetUint64(c))
	return result.Uint64()
}

// ── init ──────────────────────────────────────────────────────────────────────
// init registers all protobuf file descriptors with the FSM during the
// startup handshake. Do not modify — order and contents must stay in sync
// with the .proto files compiled into this package.

func init() {
	file_account_proto_init()
	file_event_proto_init()
	file_plugin_proto_init()
	file_tx_proto_init()

	var fds [][]byte
	for _, file := range []protoreflect.FileDescriptor{
		anypb.File_google_protobuf_any_proto,
		File_account_proto, File_event_proto, File_plugin_proto, File_tx_proto,
	} {
		// Panic on marshal failure instead of silently appending a nil entry.
		// A bad file descriptor causes a confusing FSM handshake failure —
		// better to fail loud and early.
		fd, err := proto.Marshal(protodesc.ToFileDescriptorProto(file))
		if err != nil {
			panic("praxis: failed to marshal file descriptor proto: " + err.Error())
		}
		fds = append(fds, fd)
	}
	ContractConfig.FileDescriptorProtos = fds
}

// ── Contract struct ───────────────────────────────────────────────────────────

// Contract implements the Praxis prediction-market application logic.
// currentHeight is captured in BeginBlock so DeliverTx handlers can
// reference the current block height (PluginDeliverRequest does not carry it).
type Contract struct {
	Config        Config
	FSMConfig     *PluginFSMConfig
	plugin        *Plugin
	fsmId         uint64
	currentHeight uint64 // set in BeginBlock; read in DeliverTx handlers
}

// ── Lifecycle ─────────────────────────────────────────────────────────────────

// Genesis imports initial state from a JSON file at chain launch (height 0).
func (c *Contract) Genesis(_ *PluginGenesisRequest) *PluginGenesisResponse {
	return &PluginGenesisResponse{}
}

// BeginBlock is called at the start of every block before any transactions
// are processed. We capture the block height here so DeliverTx handlers
// can reference it — PluginDeliverRequest does not carry a height field.
func (c *Contract) BeginBlock(req *PluginBeginRequest) *PluginBeginResponse {
	c.currentHeight = req.Height
	return &PluginBeginResponse{}
}

// EndBlock is called at the end of every block after all transactions have
// been applied.
func (c *Contract) EndBlock(_ *PluginEndRequest) *PluginEndResponse {
	return &PluginEndResponse{}
}

// ── CheckTx ───────────────────────────────────────────────────────────────────
// CheckTx is stateless validation. It runs when a transaction enters the
// mempool on every node. It CANNOT write state. State reads should be
// minimal (the default template reads fee params here, which is acceptable
// per the Canopy plugin spec).

func (c *Contract) CheckTx(request *PluginCheckRequest) *PluginCheckResponse {
	// Read minimum fee parameters from state — this is the one state read
	// permitted in CheckTx per the default template pattern.
	resp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: nextQueryId(), Key: KeyForFeeParams()},
		},
	})
	// StateRead returns transport error as second value AND may embed error
	// in resp.Error — check both per the plugin spec.
	if err == nil {
		err = resp.Error
	}
	if err != nil {
		return &PluginCheckResponse{Error: err}
	}

	// FIX (Bug 1): Guard against empty Results or empty Entries before
	// indexing. On a fresh chain before genesis sets FeeParams, this slice
	// is empty and the previous code panicked with an index-out-of-range.
	// When FeeParams is absent we default to a zero struct (no minimum fee).
	minFees := new(FeeParams)
	if len(resp.Results) > 0 && len(resp.Results[0].Entries) > 0 {
		if err = Unmarshal(resp.Results[0].Entries[0].Value, minFees); err != nil {
			return &PluginCheckResponse{Error: err}
		}
	}

	// Route fee check to the correct minimum fee per tx type.
	msg, err := FromAny(request.Tx.Msg)
	if err != nil {
		return &PluginCheckResponse{Error: err}
	}

	var minFee uint64
	switch msg.(type) {
	case *MessageSend:
		minFee = minFees.SendFee
	case *MessageCreateMarket:
		minFee = minFees.CreateMarketFee
	case *MessageSubmitPrediction:
		minFee = minFees.SubmitPredictionFee
	case *MessageResolveMarket:
		minFee = minFees.ResolveMarketFee
	case *MessageClaimWinnings:
		minFee = minFees.ClaimWinningsFee
	default:
		return &PluginCheckResponse{Error: ErrInvalidMessageCast()}
	}
	if request.Tx.Fee < minFee {
		return &PluginCheckResponse{Error: ErrTxFeeBelowStateLimit()}
	}

	switch x := msg.(type) {
	case *MessageSend:
		return c.CheckMessageSend(x)
	case *MessageCreateMarket:
		return c.CheckCreateMarket(x)
	case *MessageSubmitPrediction:
		return c.CheckSubmitPrediction(x)
	case *MessageResolveMarket:
		return c.CheckResolveMarket(x)
	case *MessageClaimWinnings:
		return c.CheckClaimWinnings(x)
	default:
		return &PluginCheckResponse{Error: ErrInvalidMessageCast()}
	}
}

// ── DeliverTx ─────────────────────────────────────────────────────────────────
// DeliverTx is called exactly once per transaction when a block is applied.
// It can read and write state. If it returns an error the transaction is
// still recorded in the block as failed and the fee is still charged —
// CheckTx must catch everything recoverable before a tx enters a block.

func (c *Contract) DeliverTx(request *PluginDeliverRequest) *PluginDeliverResponse {
	msg, err := FromAny(request.Tx.Msg)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	switch x := msg.(type) {
	case *MessageSend:
		return c.DeliverMessageSend(x, request.Tx.Fee)
	case *MessageCreateMarket:
		return c.DeliverCreateMarket(x, request.Tx.Fee)
	case *MessageSubmitPrediction:
		return c.DeliverSubmitPrediction(x, request.Tx.Fee)
	case *MessageResolveMarket:
		return c.DeliverResolveMarket(x, request.Tx.Fee)
	case *MessageClaimWinnings:
		return c.DeliverClaimWinnings(x, request.Tx.Fee)
	default:
		return &PluginDeliverResponse{Error: ErrInvalidMessageCast()}
	}
}

// ── CheckTx handlers ──────────────────────────────────────────────────────────
// Each handler validates the message stateless-ly and returns the
// AuthorizedSigners whose BLS signatures the FSM must verify.

func (c *Contract) CheckMessageSend(msg *MessageSend) *PluginCheckResponse {
	if len(msg.FromAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if len(msg.ToAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.Amount == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{
		Recipient:         msg.ToAddress,
		AuthorizedSigners: [][]byte{msg.FromAddress},
	}
}

func (c *Contract) CheckCreateMarket(msg *MessageCreateMarket) *PluginCheckResponse {
	if len(msg.CreatorAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if len(msg.ResolverAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	// Prevent creator from also being the resolver — the simplest manipulation
	// vector. A creator who resolves their own market can always pick the
	// outcome that suits them.
	if bytes.Equal(msg.CreatorAddress, msg.ResolverAddress) {
		return &PluginCheckResponse{Error: ErrCreatorIsResolver()}
	}
	if msg.Question == "" {
		return &PluginCheckResponse{Error: ErrEmptyQuestion()}
	}
	if msg.StakeAmount == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	// ResolutionHeight range enforcement is in DeliverCreateMarket —
	// currentHeight is unavailable here.
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.CreatorAddress}}
}

func (c *Contract) CheckSubmitPrediction(msg *MessageSubmitPrediction) *PluginCheckResponse {
	if len(msg.ForecasterAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.MarketId == 0 {
		return &PluginCheckResponse{Error: ErrInvalidMarketId()}
	}
	if msg.Outcome != 1 && msg.Outcome != 2 {
		return &PluginCheckResponse{Error: ErrInvalidOutcome()}
	}
	if msg.Amount == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.ForecasterAddress}}
}

func (c *Contract) CheckResolveMarket(msg *MessageResolveMarket) *PluginCheckResponse {
	if len(msg.ResolverAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.MarketId == 0 {
		return &PluginCheckResponse{Error: ErrInvalidMarketId()}
	}
	if msg.WinningOutcome != 1 && msg.WinningOutcome != 2 {
		return &PluginCheckResponse{Error: ErrInvalidOutcome()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.ResolverAddress}}
}

func (c *Contract) CheckClaimWinnings(msg *MessageClaimWinnings) *PluginCheckResponse {
	if len(msg.ClaimerAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.MarketId == 0 {
		return &PluginCheckResponse{Error: ErrInvalidMarketId()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.ClaimerAddress}}
}

// ── DeliverTx: send ───────────────────────────────────────────────────────────

func (c *Contract) DeliverMessageSend(msg *MessageSend, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverMessageSend: from=%x to=%x amount=%d fee=%d",
		msg.FromAddress, msg.ToAddress, msg.Amount, fee)

	fromQueryId := nextQueryId()
	toQueryId   := nextQueryId()
	feeQueryId  := nextQueryId()

	fromKey    := KeyForAccount(msg.FromAddress)
	toKey      := KeyForAccount(msg.ToAddress)
	feePoolKey := KeyForFeePool(c.Config.ChainId)

	isSelfTransfer := bytes.Equal(fromKey, toKey)

	// For self-transfers, only read one key to avoid the pointer-aliasing
	// problem entirely. We handle the accounting separately.
	var readKeys []*PluginKeyRead
	if isSelfTransfer {
		readKeys = []*PluginKeyRead{
			{QueryId: feeQueryId, Key: feePoolKey},
			{QueryId: fromQueryId, Key: fromKey},
		}
	} else {
		readKeys = []*PluginKeyRead{
			{QueryId: feeQueryId, Key: feePoolKey},
			{QueryId: fromQueryId, Key: fromKey},
			{QueryId: toQueryId, Key: toKey},
		}
	}

	response, err := c.plugin.StateRead(c, &PluginStateReadRequest{Keys: readKeys})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if response.Error != nil {
		return &PluginDeliverResponse{Error: response.Error}
	}

	from    := new(Account)
	to      := new(Account)
	feePool := new(Pool)
	var fromBytes, toBytes, feePoolBytes []byte

	for _, r := range response.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case fromQueryId:
			fromBytes = r.Entries[0].Value
		case toQueryId:
			toBytes = r.Entries[0].Value
		case feeQueryId:
			feePoolBytes = r.Entries[0].Value
		}
	}

	if err = Unmarshal(fromBytes, from); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	// Self-transfer: sender pays only the fee. Amount goes back to the same account.
	if isSelfTransfer {
		if from.Amount < fee {
			return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
		}
		from.Amount -= fee
		feePool.Amount += fee

		fromBytes, err = Marshal(from)
		if err != nil {
			return &PluginDeliverResponse{Error: err}
		}
		feePoolBytes, err = Marshal(feePool)
		if err != nil {
			return &PluginDeliverResponse{Error: err}
		}

		var writeResp *PluginStateWriteResponse
		if from.Amount == 0 {
			writeResp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
				Sets:    []*PluginSetOp{{Key: feePoolKey, Value: feePoolBytes}},
				Deletes: []*PluginDeleteOp{{Key: fromKey}},
			})
		} else {
			writeResp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
				Sets: []*PluginSetOp{
					{Key: feePoolKey, Value: feePoolBytes},
					{Key: fromKey, Value: fromBytes},
				},
			})
		}
		if err != nil {
			return &PluginDeliverResponse{Error: err}
		}
		if writeResp.Error != nil {
			return &PluginDeliverResponse{Error: writeResp.Error}
		}
		return &PluginDeliverResponse{}
	}

	// Normal transfer: deduct amount + fee from sender, credit amount to receiver.
	if err = Unmarshal(toBytes, to); err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	amountToDeduct := msg.Amount + fee
	if from.Amount < amountToDeduct {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	from.Amount -= amountToDeduct
	feePool.Amount += fee
	to.Amount += msg.Amount

	fromBytes, err = Marshal(from)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	toBytes, err = Marshal(to)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feePoolBytes, err = Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	// If the sender account is drained, delete it to keep state minimal.
	var writeResp *PluginStateWriteResponse
	if from.Amount == 0 {
		writeResp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
			Sets: []*PluginSetOp{
				{Key: feePoolKey, Value: feePoolBytes},
				{Key: toKey, Value: toBytes},
			},
			Deletes: []*PluginDeleteOp{{Key: fromKey}},
		})
	} else {
		writeResp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
			Sets: []*PluginSetOp{
				{Key: feePoolKey, Value: feePoolBytes},
				{Key: toKey, Value: toBytes},
				{Key: fromKey, Value: fromBytes},
			},
		})
	}
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	return &PluginDeliverResponse{}
}

// ── DeliverTx: create_market ──────────────────────────────────────────────────

func (c *Contract) DeliverCreateMarket(msg *MessageCreateMarket, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverCreateMarket: creator=%x question=%q stakeAmount=%d height=%d",
		msg.CreatorAddress, msg.Question, msg.StakeAmount, c.currentHeight)

	// ResolutionHeight must be at least MinMarketDuration blocks in the future.
	// This covers both "in the past" and "too soon" cases.
	if msg.ResolutionHeight < c.currentHeight+MinMarketDuration {
		return &PluginDeliverResponse{Error: ErrInvalidResolutionHeight()}
	}
	// FIX (Security Issue 5): ResolutionHeight must not exceed MaxMarketDuration
	// blocks in the future. Without this cap, a creator can lock all forecaster
	// funds for an effectively infinite duration.
	if msg.ResolutionHeight > c.currentHeight+MaxMarketDuration {
		return &PluginDeliverResponse{Error: ErrInvalidResolutionHeight()}
	}

	counterQId := nextQueryId()
	creatorQId := nextQueryId()
	feeQId     := nextQueryId()

	counterKey := KeyForMarketCounter()
	creatorKey := KeyForAccount(msg.CreatorAddress)
	feePoolKey := KeyForFeePool(c.Config.ChainId)

	readResp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: counterQId, Key: counterKey},
			{QueryId: creatorQId, Key: creatorKey},
			{QueryId: feeQId, Key: feePoolKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if readResp.Error != nil {
		return &PluginDeliverResponse{Error: readResp.Error}
	}

	counter := new(MarketCounter)
	creator := new(Account)
	feePool := new(Pool)
	var counterBytes, creatorBytes, feePoolBytes []byte

	for _, r := range readResp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case counterQId:
			counterBytes = r.Entries[0].Value
		case creatorQId:
			creatorBytes = r.Entries[0].Value
		case feeQId:
			feePoolBytes = r.Entries[0].Value
		}
	}

	if err = Unmarshal(counterBytes, counter); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(creatorBytes, creator); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	// The creator pays the tx fee only. The stake bond is escrowed inside the
	// Market struct (StakeBond field) and is NOT added to YesPool/NoPool —
	// it is a separate returnable bond, returned in DeliverResolveMarket.
	totalCost := msg.StakeAmount + fee
	if creator.Amount < totalCost {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	counter.Count++
	newMarket := &Market{
		Id:               counter.Count,
		CreatorAddress:   msg.CreatorAddress,
		Question:         msg.Question,
		Description:      msg.Description,
		ResolverAddress:  msg.ResolverAddress,
		ResolutionHeight: msg.ResolutionHeight,
		StakeAmount:      msg.StakeAmount,
		StakeBond:        msg.StakeAmount, // escrowed bond — returned on resolution
		YesPool:          0,
		NoPool:           0,
		Status:           0, // 0 = open
		WinningOutcome:   0,
	}

	// Deduct full cost (stake bond + fee) from creator.
	// Fee goes to fee pool. Stake bond is locked inside the market record.
	creator.Amount -= totalCost
	feePool.Amount += fee

	marketBytes, err := Marshal(newMarket)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	counterBytes, err = Marshal(counter)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	creatorBytes, err = Marshal(creator)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feePoolBytes, err = Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: KeyForMarket(newMarket.Id), Value: marketBytes},
			{Key: counterKey, Value: counterBytes},
			{Key: creatorKey, Value: creatorBytes},
			{Key: feePoolKey, Value: feePoolBytes},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	log.Printf("Market #%d created: %q stakeBond=%d", newMarket.Id, newMarket.Question, newMarket.StakeBond)
	return &PluginDeliverResponse{}
}

// ── DeliverTx: submit_prediction ─────────────────────────────────────────────

func (c *Contract) DeliverSubmitPrediction(msg *MessageSubmitPrediction, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverSubmitPrediction: forecaster=%x marketId=%d outcome=%d amount=%d",
		msg.ForecasterAddress, msg.MarketId, msg.Outcome, msg.Amount)

	marketQId     := nextQueryId()
	forecasterQId := nextQueryId()
	feeQId        := nextQueryId()
	predQId       := nextQueryId()

	marketKey     := KeyForMarket(msg.MarketId)
	forecasterKey := KeyForAccount(msg.ForecasterAddress)
	feePoolKey    := KeyForFeePool(c.Config.ChainId)
	predKey       := KeyForPrediction(msg.ForecasterAddress, msg.MarketId)

	readResp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: marketQId, Key: marketKey},
			{QueryId: forecasterQId, Key: forecasterKey},
			{QueryId: feeQId, Key: feePoolKey},
			{QueryId: predQId, Key: predKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if readResp.Error != nil {
		return &PluginDeliverResponse{Error: readResp.Error}
	}

	market       := new(Market)
	forecaster   := new(Account)
	feePool      := new(Pool)
	existingPred := new(Prediction)
	var marketBytes, forecasterBytes, feePoolBytes, existingPredBytes []byte

	for _, r := range readResp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case marketQId:
			marketBytes = r.Entries[0].Value
		case forecasterQId:
			forecasterBytes = r.Entries[0].Value
		case feeQId:
			feePoolBytes = r.Entries[0].Value
		case predQId:
			existingPredBytes = r.Entries[0].Value
		}
	}

	if err = Unmarshal(marketBytes, market); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if market.Id == 0 {
		return &PluginDeliverResponse{Error: ErrMarketNotFound()}
	}
	if market.Status != 0 {
		return &PluginDeliverResponse{Error: ErrMarketClosed()}
	}

	// FIX (Logic Issue 2): Predictions must not be accepted on or after the
	// resolution height. Without this check, a forecaster watching mempool
	// could submit after seeing how real-world events unfolded while the
	// market is technically still status=0/open.
	if c.currentHeight >= market.ResolutionHeight {
		return &PluginDeliverResponse{Error: ErrMarketClosed()}
	}

	// FIX (Security Issue 1): Block the creator from predicting on their own
	// market. The creator knows the question and may have information
	// advantage or indirect influence over the outcome.
	if bytes.Equal(market.CreatorAddress, msg.ForecasterAddress) {
		return &PluginDeliverResponse{Error: ErrCreatorCannotPredict()}
	}

	// FIX (Security Issue 2): Block the resolver from predicting on any market
	// they are designated to resolve. The resolver decides the outcome — they
	// must never have a financial stake in either side.
	if bytes.Equal(market.ResolverAddress, msg.ForecasterAddress) {
		return &PluginDeliverResponse{Error: ErrResolverCannotPredict()}
	}

	// Prevent duplicate predictions per forecaster per market.
	if err = Unmarshal(existingPredBytes, existingPred); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if existingPred.MarketId != 0 {
		return &PluginDeliverResponse{Error: ErrDuplicatePrediction()}
	}

	if err = Unmarshal(forecasterBytes, forecaster); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	totalCost := msg.Amount + fee
	if forecaster.Amount < totalCost {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	forecaster.Amount -= totalCost
	feePool.Amount += fee

	if msg.Outcome == 1 {
		market.YesPool += msg.Amount
	} else {
		market.NoPool += msg.Amount
	}

	newPrediction := &Prediction{
		ForecasterAddress: msg.ForecasterAddress,
		MarketId:          msg.MarketId,
		Outcome:           msg.Outcome,
		Amount:            msg.Amount,
		Claimed:           false,
	}

	marketBytes, err = Marshal(market)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	forecasterBytes, err = Marshal(forecaster)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feePoolBytes, err = Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	predBytes, err := Marshal(newPrediction)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: marketKey, Value: marketBytes},
			{Key: forecasterKey, Value: forecasterBytes},
			{Key: feePoolKey, Value: feePoolBytes},
			{Key: predKey, Value: predBytes},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	log.Printf("Prediction stored: market=%d outcome=%d amount=%d",
		msg.MarketId, msg.Outcome, msg.Amount)
	return &PluginDeliverResponse{}
}

// ── DeliverTx: resolve_market ─────────────────────────────────────────────────

func (c *Contract) DeliverResolveMarket(msg *MessageResolveMarket, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverResolveMarket: resolver=%x marketId=%d winningOutcome=%d",
		msg.ResolverAddress, msg.MarketId, msg.WinningOutcome)

	marketQId   := nextQueryId()
	resolverQId := nextQueryId()
	creatorQId  := nextQueryId()
	feeQId      := nextQueryId()

	marketKey   := KeyForMarket(msg.MarketId)
	resolverKey := KeyForAccount(msg.ResolverAddress)
	feePoolKey  := KeyForFeePool(c.Config.ChainId)

	// We need the market first to get CreatorAddress for the bond return.
	// Read market alone first, then batch resolver + creator + feePool together.
	marketReadResp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: marketQId, Key: marketKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if marketReadResp.Error != nil {
		return &PluginDeliverResponse{Error: marketReadResp.Error}
	}

	market := new(Market)
	var marketBytes []byte
	for _, r := range marketReadResp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		if r.QueryId == marketQId {
			marketBytes = r.Entries[0].Value
		}
	}
	if err = Unmarshal(marketBytes, market); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if market.Id == 0 {
		return &PluginDeliverResponse{Error: ErrMarketNotFound()}
	}
	if market.Status != 0 {
		return &PluginDeliverResponse{Error: ErrMarketClosed()}
	}
	// Hard block — resolution is not permitted before the declared resolution height.
	if c.currentHeight < market.ResolutionHeight {
		return &PluginDeliverResponse{Error: ErrResolutionTooEarly()}
	}
	// Only the designated resolver address may call resolve.
	if !bytes.Equal(market.ResolverAddress, msg.ResolverAddress) {
		return &PluginDeliverResponse{Error: ErrInvalidAddress()}
	}

	// Now we know the creator address — batch the remaining reads together.
	creatorKey := KeyForAccount(market.CreatorAddress)
	readResp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: resolverQId, Key: resolverKey},
			{QueryId: creatorQId, Key: creatorKey},
			{QueryId: feeQId, Key: feePoolKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if readResp.Error != nil {
		return &PluginDeliverResponse{Error: readResp.Error}
	}

	resolver := new(Account)
	creator  := new(Account)
	feePool  := new(Pool)
	var resolverBytes, creatorBytes, feePoolBytes []byte

	for _, r := range readResp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case resolverQId:
			resolverBytes = r.Entries[0].Value
		case creatorQId:
			creatorBytes = r.Entries[0].Value
		case feeQId:
			feePoolBytes = r.Entries[0].Value
		}
	}

	if err = Unmarshal(resolverBytes, resolver); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(creatorBytes, creator); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resolver.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	// FIX (Bug 2): Capture bondReturned BEFORE zeroing StakeBond so the log
	// prints the correct value. Previously the log always printed 0.
	bondReturned := market.StakeBond

	resolver.Amount -= fee
	feePool.Amount += fee
	market.Status = 1 // 1 = resolved
	market.WinningOutcome = msg.WinningOutcome

	// Return the stake bond to the creator.
	creator.Amount += market.StakeBond
	market.StakeBond = 0 // bond has been returned

	marketBytes, err = Marshal(market)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	resolverBytes, err = Marshal(resolver)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feePoolBytes, err = Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	creatorBytes, err = Marshal(creator)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: marketKey, Value: marketBytes},
			{Key: resolverKey, Value: resolverBytes},
			{Key: feePoolKey, Value: feePoolBytes},
			{Key: creatorKey, Value: creatorBytes},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	log.Printf("Market #%d resolved: winningOutcome=%d stakeBondReturned=%d",
		market.Id, market.WinningOutcome, bondReturned)
	return &PluginDeliverResponse{}
}

// ── DeliverTx: claim_winnings ─────────────────────────────────────────────────

func (c *Contract) DeliverClaimWinnings(msg *MessageClaimWinnings, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverClaimWinnings: claimer=%x marketId=%d", msg.ClaimerAddress, msg.MarketId)

	marketQId   := nextQueryId()
	claimerQId  := nextQueryId()
	feeQId      := nextQueryId()
	predQId     := nextQueryId()
	treasuryQId := nextQueryId()

	marketKey   := KeyForMarket(msg.MarketId)
	claimerKey  := KeyForAccount(msg.ClaimerAddress)
	feePoolKey  := KeyForFeePool(c.Config.ChainId)
	predKey     := KeyForPrediction(msg.ClaimerAddress, msg.MarketId)
	treasuryKey := KeyForAccount(TreasuryAddress)

	readResp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: marketQId, Key: marketKey},
			{QueryId: claimerQId, Key: claimerKey},
			{QueryId: feeQId, Key: feePoolKey},
			{QueryId: predQId, Key: predKey},
			{QueryId: treasuryQId, Key: treasuryKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if readResp.Error != nil {
		return &PluginDeliverResponse{Error: readResp.Error}
	}

	market   := new(Market)
	claimer  := new(Account)
	feePool  := new(Pool)
	pred     := new(Prediction)
	treasury := new(Account)
	var marketBytes, claimerBytes, feePoolBytes, predBytes, treasuryBytes []byte

	for _, r := range readResp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case marketQId:
			marketBytes = r.Entries[0].Value
		case claimerQId:
			claimerBytes = r.Entries[0].Value
		case feeQId:
			feePoolBytes = r.Entries[0].Value
		case predQId:
			predBytes = r.Entries[0].Value
		case treasuryQId:
			treasuryBytes = r.Entries[0].Value
		}
	}

	if err = Unmarshal(marketBytes, market); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if market.Id == 0 {
		return &PluginDeliverResponse{Error: ErrMarketNotFound()}
	}
	// Market must be resolved before anyone can claim.
	if market.Status != 1 {
		return &PluginDeliverResponse{Error: ErrMarketNotResolved()}
	}

	if err = Unmarshal(predBytes, pred); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	// Claimer must have a prediction recorded for this market.
	if pred.MarketId == 0 {
		return &PluginDeliverResponse{Error: ErrNoPredictionFound()}
	}
	// Cannot claim more than once.
	if pred.Claimed {
		return &PluginDeliverResponse{Error: ErrAlreadyClaimed()}
	}
	// Claimer must have picked the winning outcome.
	if pred.Outcome != market.WinningOutcome {
		return &PluginDeliverResponse{Error: ErrWrongOutcome()}
	}

	if err = Unmarshal(claimerBytes, claimer); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(treasuryBytes, treasury); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if claimer.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	// Proportional payout from the winning pool + losing pool share.
	var winPool, losePool uint64
	if market.WinningOutcome == 1 {
		winPool  = market.YesPool
		losePool = market.NoPool
	} else {
		winPool  = market.NoPool
		losePool = market.YesPool
	}

	// Apply the 2% treasury cut to the losing pool BEFORE calculating winner
	// payouts. This mirrors Polymarket's revenue model.
	// FIX (Security Issue 4): Use mulDiv to prevent uint64 overflow in the
	// intermediate multiplication. losePool * TreasuryFeeBps can exceed
	// MaxUint64 for large pool sizes, silently wrapping to near-zero.
	treasuryCut     := mulDiv(losePool, TreasuryFeeBps, 10000)
	losePoolAfterCut := losePool - treasuryCut

	var payout uint64
	if winPool > 0 {
		// Winner receives their stake back plus a proportional share of the
		// losing pool (after the treasury cut).
		// FIX (Security Issue 4): mulDiv prevents overflow on pred.Amount * losePoolAfterCut.
		payout = pred.Amount + mulDiv(pred.Amount, losePoolAfterCut, winPool)
	} else {
		// Edge case: no one bet on the losing side — return original stake.
		payout = pred.Amount
	}

	claimer.Amount = claimer.Amount - fee + payout
	feePool.Amount += fee
	treasury.Amount += treasuryCut
	pred.Claimed = true

	// Update market pool balances to reflect the payout.
	// Decrement the winning pool by the claimer's stake and the losing pool
	// by their proportional share + treasury cut.
	// FIX (Security Issue 4): mulDiv prevents overflow here too.
	var claimerLoseShare uint64
	if winPool > 0 {
		claimerLoseShare = mulDiv(pred.Amount, losePoolAfterCut, winPool)
	}
	if market.WinningOutcome == 1 {
		if market.YesPool >= pred.Amount {
			market.YesPool -= pred.Amount
		} else {
			market.YesPool = 0
		}
		paidFromLose := claimerLoseShare + treasuryCut
		if market.NoPool >= paidFromLose {
			market.NoPool -= paidFromLose
		} else {
			market.NoPool = 0
		}
	} else {
		if market.NoPool >= pred.Amount {
			market.NoPool -= pred.Amount
		} else {
			market.NoPool = 0
		}
		paidFromLose := claimerLoseShare + treasuryCut
		if market.YesPool >= paidFromLose {
			market.YesPool -= paidFromLose
		} else {
			market.YesPool = 0
		}
	}

	marketBytes, err = Marshal(market)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	claimerBytes, err = Marshal(claimer)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feePoolBytes, err = Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	predBytes, err = Marshal(pred)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	treasuryBytes, err = Marshal(treasury)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: marketKey, Value: marketBytes},
			{Key: claimerKey, Value: claimerBytes},
			{Key: feePoolKey, Value: feePoolBytes},
			{Key: predKey, Value: predBytes},
			{Key: treasuryKey, Value: treasuryBytes},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	log.Printf("Winnings claimed: market=%d claimer=%x payout=%d treasuryCut=%d",
		msg.MarketId, msg.ClaimerAddress, payout, treasuryCut)
	return &PluginDeliverResponse{}
}

// ── State key prefixes ────────────────────────────────────────────────────────
//
// Byte prefixes namespace each state type in the shared key-value store.
// All keys are constructed with JoinLenPrefix which length-prefixes each
// segment, so the raw key bytes always start with a length byte (0x01 for
// a 1-byte prefix segment) followed by the prefix value.
//
// Built-in prefixes from the Canopy template — do not reuse:
//   0x01  Account
//   0x02  Pool (fee pool)
//   0x07  FeeParams
//
// Praxis-specific prefixes — must not conflict with built-ins:
//   0x10  Market
//   0x11  MarketCounter (singleton)
//   0x12  Prediction

var (
	accountPrefix       = []byte{0x01}
	poolPrefix          = []byte{0x02}
	paramsPrefix        = []byte{0x07}
	marketPrefix        = []byte{0x10}
	marketCounterPrefix = []byte{0x11}
	predictionPrefix    = []byte{0x12}
)

func KeyForAccount(addr []byte) []byte {
	return JoinLenPrefix(accountPrefix, addr)
}

func KeyForFeeParams() []byte {
	return JoinLenPrefix(paramsPrefix, []byte("/f/"))
}

func KeyForFeePool(chainId uint64) []byte {
	return JoinLenPrefix(poolPrefix, formatUint64(chainId))
}

func KeyForMarket(id uint64) []byte {
	return JoinLenPrefix(marketPrefix, formatUint64(id))
}

func KeyForMarketCounter() []byte {
	return JoinLenPrefix(marketCounterPrefix, []byte("/mc/"))
}

// KeyForPrediction builds a composite key from the forecaster address and
// market ID. We allocate a new slice explicitly to avoid mutating the
// underlying array of the addr argument (a common Go slice-append pitfall).
func KeyForPrediction(addr []byte, marketId uint64) []byte {
	idBytes := formatUint64(marketId)
	composite := make([]byte, len(addr)+8)
	copy(composite, addr)
	copy(composite[len(addr):], idBytes)
	return JoinLenPrefix(predictionPrefix, composite)
}

func formatUint64(u uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, u)
	return b
}
