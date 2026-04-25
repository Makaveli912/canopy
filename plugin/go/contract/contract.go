package contract

import (
	"bytes"
	"encoding/binary"
	"log"
	"math/rand"

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
		fd, _ := proto.Marshal(protodesc.ToFileDescriptorProto(file))
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
			{QueryId: rand.Uint64(), Key: KeyForFeeParams()},
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
	minFees := new(FeeParams)
	if err = Unmarshal(resp.Results[0].Entries[0].Value, minFees); err != nil {
		return &PluginCheckResponse{Error: err}
	}
	if request.Tx.Fee < minFees.SendFee {
		return &PluginCheckResponse{Error: ErrTxFeeBelowStateLimit()}
	}

	msg, err := FromAny(request.Tx.Msg)
	if err != nil {
		return &PluginCheckResponse{Error: err}
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
	if msg.Question == "" {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	if msg.StakeAmount == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	// resolutionHeight > currentHeight is enforced in DeliverTx because
	// CheckTx is stateless and currentHeight is not available here.
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.CreatorAddress}}
}

func (c *Contract) CheckSubmitPrediction(msg *MessageSubmitPrediction) *PluginCheckResponse {
	if len(msg.ForecasterAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.MarketId == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	if msg.Outcome != 1 && msg.Outcome != 2 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
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
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	if msg.WinningOutcome != 1 && msg.WinningOutcome != 2 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.ResolverAddress}}
}

func (c *Contract) CheckClaimWinnings(msg *MessageClaimWinnings) *PluginCheckResponse {
	if len(msg.ClaimerAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.MarketId == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.ClaimerAddress}}
}

// ── DeliverTx: send ───────────────────────────────────────────────────────────

func (c *Contract) DeliverMessageSend(msg *MessageSend, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverMessageSend: from=%x to=%x amount=%d fee=%d",
		msg.FromAddress, msg.ToAddress, msg.Amount, fee)

	var (
		fromQueryId, toQueryId, feeQueryId = rand.Uint64(), rand.Uint64(), rand.Uint64()
		fromKey                            = KeyForAccount(msg.FromAddress)
		toKey                              = KeyForAccount(msg.ToAddress)
		feePoolKey                         = KeyForFeePool(c.Config.ChainId)
		from, to, feePool                  = new(Account), new(Account), new(Pool)
		fromBytes, toBytes, feePoolBytes   []byte
	)

	response, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: feeQueryId, Key: feePoolKey},
			{QueryId: fromQueryId, Key: fromKey},
			{QueryId: toQueryId, Key: toKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if response.Error != nil {
		return &PluginDeliverResponse{Error: response.Error}
	}

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
	if err = Unmarshal(toBytes, to); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	amountToDeduct := msg.Amount + fee
	if from.Amount < amountToDeduct {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}
	// Self-transfer: use the same account object for both sides.
	if bytes.Equal(fromKey, toKey) {
		to = from
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

	// Enforce that the resolution height is in the future.
	// This cannot be done in CheckTx because currentHeight is not available there.
	if msg.ResolutionHeight <= c.currentHeight {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}

	counterQId := rand.Uint64()
	creatorQId := rand.Uint64()
	feeQId     := rand.Uint64()

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

	// Creator must be able to afford the stake bond plus the transaction fee.
	totalCost := msg.StakeAmount + fee
	if creator.Amount < totalCost {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	// Assign the next market ID by incrementing the counter singleton.
	counter.Count++
	newMarket := &Market{
		Id:               counter.Count,
		CreatorAddress:   msg.CreatorAddress,
		Question:         msg.Question,
		Description:      msg.Description,
		ResolverAddress:  msg.ResolverAddress,
		ResolutionHeight: msg.ResolutionHeight,
		StakeAmount:      msg.StakeAmount,
		YesPool:          0,
		NoPool:           0,
		Status:           0, // 0 = open
		WinningOutcome:   0,
	}

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
	log.Printf("Market #%d created: %q", newMarket.Id, newMarket.Question)
	return &PluginDeliverResponse{}
}

// ── DeliverTx: submit_prediction ─────────────────────────────────────────────

func (c *Contract) DeliverSubmitPrediction(msg *MessageSubmitPrediction, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverSubmitPrediction: forecaster=%x marketId=%d outcome=%d amount=%d",
		msg.ForecasterAddress, msg.MarketId, msg.Outcome, msg.Amount)

	marketQId     := rand.Uint64()
	forecasterQId := rand.Uint64()
	feeQId        := rand.Uint64()
	predQId       := rand.Uint64() // read existing prediction to prevent duplicates

	marketKey     := KeyForMarket(msg.MarketId)
	forecasterKey := KeyForAccount(msg.ForecasterAddress)
	feePoolKey    := KeyForFeePool(c.Config.ChainId)
	predKey       := KeyForPrediction(msg.ForecasterAddress, msg.MarketId)

	// Batch-read all relevant keys in one call per the plugin spec pattern.
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

	market          := new(Market)
	forecaster      := new(Account)
	feePool         := new(Pool)
	existingPred    := new(Prediction)
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
	// Market must exist.
	if market.Id == 0 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	// Market must still be open (status 0).
	if market.Status != 0 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}

	// FIX: Prevent duplicate predictions. A forecaster may only submit once
	// per market. Without this guard the second submission silently overwrites
	// the first, losing the original stake from the pool accounting.
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

	marketQId   := rand.Uint64()
	resolverQId := rand.Uint64()
	feeQId      := rand.Uint64()

	marketKey   := KeyForMarket(msg.MarketId)
	resolverKey := KeyForAccount(msg.ResolverAddress)
	feePoolKey  := KeyForFeePool(c.Config.ChainId)

	readResp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: marketQId, Key: marketKey},
			{QueryId: resolverQId, Key: resolverKey},
			{QueryId: feeQId, Key: feePoolKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if readResp.Error != nil {
		return &PluginDeliverResponse{Error: readResp.Error}
	}

	market   := new(Market)
	resolver := new(Account)
	feePool  := new(Pool)
	var marketBytes, resolverBytes, feePoolBytes []byte

	for _, r := range readResp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case marketQId:
			marketBytes = r.Entries[0].Value
		case resolverQId:
			resolverBytes = r.Entries[0].Value
		case feeQId:
			feePoolBytes = r.Entries[0].Value
		}
	}

	if err = Unmarshal(marketBytes, market); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	// Market must exist.
	if market.Id == 0 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	// Market must still be open.
	if market.Status != 0 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	// Only the designated resolver address may call resolve.
	if !bytes.Equal(market.ResolverAddress, msg.ResolverAddress) {
		return &PluginDeliverResponse{Error: ErrInvalidAddress()}
	}

	if err = Unmarshal(resolverBytes, resolver); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resolver.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	resolver.Amount -= fee
	feePool.Amount += fee
	market.Status = 1 // 1 = resolved
	market.WinningOutcome = msg.WinningOutcome

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

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: marketKey, Value: marketBytes},
			{Key: resolverKey, Value: resolverBytes},
			{Key: feePoolKey, Value: feePoolBytes},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	log.Printf("Market #%d resolved: winningOutcome=%d", market.Id, market.WinningOutcome)
	return &PluginDeliverResponse{}
}

// ── DeliverTx: claim_winnings ─────────────────────────────────────────────────

func (c *Contract) DeliverClaimWinnings(msg *MessageClaimWinnings, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverClaimWinnings: claimer=%x marketId=%d", msg.ClaimerAddress, msg.MarketId)

	marketQId  := rand.Uint64()
	claimerQId := rand.Uint64()
	feeQId     := rand.Uint64()
	predQId    := rand.Uint64()

	marketKey  := KeyForMarket(msg.MarketId)
	claimerKey := KeyForAccount(msg.ClaimerAddress)
	feePoolKey := KeyForFeePool(c.Config.ChainId)
	predKey    := KeyForPrediction(msg.ClaimerAddress, msg.MarketId)

	readResp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: marketQId, Key: marketKey},
			{QueryId: claimerQId, Key: claimerKey},
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

	market     := new(Market)
	claimer    := new(Account)
	feePool    := new(Pool)
	prediction := new(Prediction)
	var marketBytes, claimerBytes, feePoolBytes, predBytes []byte

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
		}
	}

	if err = Unmarshal(marketBytes, market); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	// Market must exist.
	if market.Id == 0 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	// Market must be resolved (status 1) before anyone can claim.
	if market.Status != 1 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}

	if err = Unmarshal(predBytes, prediction); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	// Claimer must have a prediction recorded for this market.
	if prediction.MarketId == 0 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	// Cannot claim more than once.
	if prediction.Claimed {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	// FIX: use ErrWrongOutcome instead of ErrInsufficientFunds for semantic
	// correctness — the claimer picked the losing side, not a funds issue.
	if prediction.Outcome != market.WinningOutcome {
		return &PluginDeliverResponse{Error: ErrWrongOutcome()}
	}

	if err = Unmarshal(claimerBytes, claimer); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if claimer.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	// Proportional payout: winner gets their stake back plus a share of
	// the losing pool proportional to their contribution to the winning pool.
	var winPool, losePool uint64
	if market.WinningOutcome == 1 {
		winPool  = market.YesPool
		losePool = market.NoPool
	} else {
		winPool  = market.NoPool
		losePool = market.YesPool
	}

	var payout uint64
	if winPool > 0 {
		payout = prediction.Amount + (prediction.Amount*losePool)/winPool
	} else {
		// Edge case: no one bet on the losing side — return original stake.
		payout = prediction.Amount
	}

	claimer.Amount = claimer.Amount - fee + payout
	feePool.Amount += fee
	prediction.Claimed = true

	claimerBytes, err = Marshal(claimer)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feePoolBytes, err = Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	predBytes, err = Marshal(prediction)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: claimerKey, Value: claimerBytes},
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
	log.Printf("Winnings claimed: market=%d claimer=%x payout=%d",
		msg.MarketId, msg.ClaimerAddress, payout)
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
