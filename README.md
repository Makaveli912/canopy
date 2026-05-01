# PRAXIS

<div align="center">

```
██████╗ ██████╗  █████╗ ██╗  ██╗██╗███████╗
██╔══██╗██╔══██╗██╔══██╗╚██╗██╔╝██║██╔════╝
██████╔╝██████╔╝███████║ ╚███╔╝ ██║███████╗
██╔═══╝ ██╔══██╗██╔══██║ ██╔██╗ ██║╚════██║
██║     ██║  ██║██║  ██║██╔╝ ██╗██║███████║
╚═╝     ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝╚══════╝
```

**On-Chain Prediction Markets on the Canopy Network**

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go)](https://go.dev)
[![Canopy](https://img.shields.io/badge/Canopy-Betanet-00ff88)](https://canopynetwork.org)
[![Plugin](https://img.shields.io/badge/Plugin-Go-00d4ff)](plugin/go)
[![Status](https://img.shields.io/badge/Status-Betanet-ffc940)](https://canopynetwork.org)

</div>

---

## ▶ Praxis is a sovereign prediction market protocol built as a Canopy Nested Chain

Praxis ($PRX) lets anyone create YES/NO prediction markets, stake on outcomes, and claim proportional winnings — entirely on-chain, with no platform extraction and no central authority. It is implemented as a Go plugin on the Canopy Network, meaning it runs as an application-specific blockchain with its own state, its own token, and its own transaction types.

[![architecture](https://img.shields.io/badge/architecture-appchain-00ff88)]()
[![consensus](https://img.shields.io/badge/consensus-NestBFT-00d4ff)]()
[![signing](https://img.shields.io/badge/signing-BLS12--381-b48eff)]()
[![state](https://img.shields.io/badge/state-key--value-ffc940)]()

---

## Overview

Praxis implements four on-chain transaction types:

| Transaction | Description |
|---|---|
| `create_market` | Open a new YES/NO prediction market with a question, resolver, and resolution height |
| `submit_prediction` | Stake tokens on a YES or NO outcome for an open market |
| `resolve_market` | The designated resolver finalises a market with the winning outcome |
| `claim_winnings` | Winners claim their proportional payout from the resolved market pool |

All state is stored on-chain in the plugin's key-value store. No database, no backend, no off-chain oracle required for basic operation.

---

## Architecture

Praxis follows the standard Canopy plugin architecture. The plugin runs as a separate process alongside the Canopy node and communicates over a Unix socket using Protocol Buffers.

```
┌─────────────────────────────────────────┐
│           CANOPY NODE PROCESS           │
│                                         │
│  ┌──────────┐  ┌────────────────────┐   │
│  │ NestBFT  │  │   FSM / Controller │   │
│  │ Consensus│  │  (block lifecycle) │   │
│  └──────────┘  └────────┬───────────┘   │
│                         │ Unix socket   │
└─────────────────────────┼───────────────┘
                          │
          ┌───────────────▼──────────────┐
          │        PRAXIS PLUGIN         │
          │                              │
          │  Genesis()                   │
          │  BeginBlock()                │
          │  CheckTx()   ← validate      │
          │  DeliverTx() ← execute       │
          │  EndBlock()                  │
          │                              │
          │  Transactions:               │
          │  - create_market             │
          │  - submit_prediction         │
          │  - resolve_market            │
          │  - claim_winnings            │
          └──────────────────────────────┘
                          ▲
                          │ HTTP RPC :50002 / :50003
                          │
          ┌───────────────┴──────────────┐
          │     PRAXIS FRONTEND          │
          │  Single-file HTML/JS         │
          │  BLS12-381 signing           │
          │  Hand-encoded protobuf       │
          └──────────────────────────────┘
```

---

## Repository Layout

```
plugin/go/
├── main.go                  # Entry point — calls contract.StartPlugin()
├── chain.json               # Chain metadata: name, symbol, chainId, networkId
├── Makefile                 # Build targets
├── pluginctl.sh             # Plugin lifecycle (start/stop/restart/status)
├── AGENTS.md                # AI assistant context for this plugin
│
├── contract/
│   ├── contract.go          # Application logic — all transaction handlers
│   ├── errors.go            # Error codes (built-in 1–14, Praxis 15–29)
│   ├── plugin.go            # Socket protocol — do not modify
│   └── tx.pb.go             # Generated Go structs from tx.proto
│
└── proto/
    ├── tx.proto             # Transaction and state message definitions
    ├── account.proto        # Account and Pool types
    ├── plugin.proto         # FSM communication protocol
    └── _generate.sh         # Regenerates Go structs from .proto files

frontend/
└── index.html               # Single-file frontend — no build step required
```

---

## State Model

Praxis stores all on-chain data in the Canopy key-value store using byte-prefixed keys:

| Prefix | Type | Description |
|---|---|---|
| `0x10` | `Market` | One record per prediction market |
| `0x11` | `MarketCounter` | Singleton — tracks the next market ID |
| `0x12` | `Prediction` | One record per (forecaster, market) pair |

Built-in Canopy prefixes (`0x01` Account, `0x02` Pool, `0x07` FeeParams) are preserved unchanged.

---

## Transaction Types

### create_market

Opens a new YES/NO prediction market. The creator bonds a stake amount and designates a resolver address. The creator and resolver must be different addresses. The market remains open for predictions until the resolution height is reached.

Resolution height must be at least `MinMarketDuration` (100) blocks in the future and no more than `MaxMarketDuration` (1,000,000) blocks in the future.

```protobuf
message MessageCreateMarket {
  bytes  creator_address   = 1;
  string question          = 2;
  string description       = 3;
  bytes  resolver_address  = 4;
  uint64 resolution_height = 5;
  uint64 stake_amount      = 6;
}
```

### submit_prediction

Stakes tokens on a YES (outcome=1) or NO (outcome=2) outcome. Each forecaster may only submit one prediction per market. The staked amount is added to the corresponding pool. The market creator and designated resolver are blocked from predicting on their own markets.

Predictions are accepted only while the market is open (`status=0`) and before `resolutionHeight` is reached.

```protobuf
message MessageSubmitPrediction {
  bytes  forecaster_address = 1;
  uint64 market_id          = 2;
  uint32 outcome            = 3;
  uint64 amount             = 4;
}
```

### resolve_market

Finalises the market. Only the designated resolver address may call this, and only on or after the declared `resolutionHeight`. Sets the winning outcome and closes the market to further predictions.

```protobuf
message MessageResolveMarket {
  bytes  resolver_address = 1;
  uint64 market_id        = 2;
  uint32 winning_outcome  = 3;
}
```

### claim_winnings

Pays out a winner's original stake plus their proportional share of the losing pool (after the 2% treasury cut). Each prediction can only be claimed once.

```protobuf
message MessageClaimWinnings {
  bytes  claimer_address = 1;
  uint64 market_id       = 2;
}
```

Payout formula:
```
treasuryCut      = losingPool × 2%
losingPoolNet    = losingPool − treasuryCut
payout           = stake + (stake × losingPoolNet) / winningPool
```

---

## Getting Started

### Prerequisites

- Go 1.24 or later
- `protoc` and `protoc-gen-go` (for proto regeneration only)
- A running Canopy node

See the [Canopy Builder Docs](https://canopynetwork.org) for full prerequisites.

### Build

```bash
# Clone and switch to the Praxis branch
git clone https://github.com/Makaveli912/canopy.git
cd canopy
git checkout feat/praxis-prediction-markets

# Build the Canopy node binary
go build -o ~/go/bin/canopy ./cmd/main

# Build the Praxis plugin binary
cd plugin/go
go build -o go-plugin .
```

### Run

```bash
# From repo root
canopy start
```

Watch for:
```
Plugin go started: go-plugin started successfully
Plugin service listening on socket: /tmp/plugin/plugin.sock
```

### Frontend

```bash
python3 -m http.server 8080 --directory frontend
```

Open `http://localhost:8080`. Go to **Node** → set host to `localhost` → Apply. The green dot confirms connection.

Go to **Signer** → paste your BLS12-381 private key → Load Key. Your address will be auto-derived and filled into all transaction forms.

---

## Payout Model

Praxis uses an AMM-style proportional payout. Winners split the losing pool (net of the 2% treasury cut) in proportion to their contribution to the winning pool.

```
Example:
  YES pool: 600,000 μPRX (forecaster contributed 200,000)
  NO pool:  400,000 μPRX (losing side)

  Treasury cut (2%):    400,000 × 2% = 8,000 μPRX
  Losing pool net:      400,000 − 8,000 = 392,000 μPRX

  Forecaster share of YES pool: 200,000 / 600,000 = 33.3%
  Forecaster payout: 200,000 + (392,000 × 33.3%) = 330,533 μPRX
```

If no one bet on the losing side, the winner's original stake is returned unchanged.

---

## Protocol Constants

| Constant | Value | Description |
|---|---|---|
| `MinMarketDuration` | 100 blocks | Minimum gap between creation and resolution height |
| `MaxMarketDuration` | 1,000,000 blocks | Maximum gap — prevents indefinitely locked bonds |
| `TreasuryFeeBps` | 200 (2%) | Protocol cut taken from the losing pool on each claim |

---

## Security Model

Praxis enforces the following constraints to prevent market manipulation:

| Rule | Where enforced |
|---|---|
| Creator ≠ Resolver | `CheckCreateMarket` |
| Creator cannot predict on own market | `DeliverSubmitPrediction` |
| Resolver cannot predict on markets they resolve | `DeliverSubmitPrediction` |
| Predictions blocked at or after `resolutionHeight` | `DeliverSubmitPrediction` |
| Resolver blocked before `resolutionHeight` | `DeliverResolveMarket` |
| One prediction per (forecaster, market) pair | `DeliverSubmitPrediction` |
| One claim per prediction | `DeliverClaimWinnings` |
| Payout arithmetic uses overflow-safe `mulDiv` | `DeliverClaimWinnings` |

---

## Error Codes

| Code | Name | Description |
|---|---|---|
| 1 | `ErrPluginTimeout` | Plugin did not respond within timeout |
| 2 | `ErrMarshal` | Failed to serialize a protobuf message |
| 3 | `ErrUnmarshal` | Failed to deserialize a protobuf message |
| 4 | `ErrFailedPluginRead` | State read operation failed |
| 5 | `ErrFailedPluginWrite` | State write operation failed |
| 6 | `ErrInvalidPluginRespId` | Response ID did not match any pending request |
| 7 | `ErrUnexpectedFSMToPlugin` | FSM sent an unexpected message type |
| 8 | `ErrInvalidFSMToPluginMMessage` | FSM message could not be parsed |
| 9 | `ErrInsufficientFunds` | Sender balance is below the required amount |
| 10 | `ErrFromAny` | Failed to unpack a protobuf Any value |
| 11 | `ErrInvalidMessageCast` | Message type did not match any registered type |
| 12 | `ErrInvalidAddress` | Address is not exactly 20 bytes |
| 13 | `ErrInvalidAmount` | Amount is zero or otherwise invalid |
| 14 | `ErrTxFeeBelowStateLimit` | Transaction fee is below the minimum set in state |
| 15 | `ErrWrongOutcome` | Claimer's prediction did not match the winning outcome |
| 16 | `ErrDuplicatePrediction` | Forecaster already submitted a prediction for this market |
| 17 | `ErrEmptyQuestion` | Market question must not be empty |
| 18 | `ErrInvalidOutcome` | Outcome must be 1 (YES) or 2 (NO) |
| 19 | `ErrMarketNotFound` | Market ID does not exist in state |
| 20 | `ErrMarketClosed` | Market is not open (resolved, cancelled, or past resolution height) |
| 21 | `ErrResolutionTooEarly` | Resolution height has not been reached yet |
| 22 | `ErrMarketNotResolved` | Market has not been resolved yet |
| 23 | `ErrNoPredictionFound` | No prediction found for this claimer and market |
| 24 | `ErrAlreadyClaimed` | Winnings have already been claimed for this prediction |
| 25 | `ErrCreatorIsResolver` | Creator and resolver must be different addresses |
| 26 | `ErrCreatorCannotPredict` | Market creator cannot predict on their own market |
| 27 | `ErrResolverCannotPredict` | Designated resolver cannot predict on markets they will resolve |
| 28 | `ErrInvalidMarketId` | Market ID must be non-zero |
| 29 | `ErrInvalidResolutionHeight` | Resolution height out of allowed range |

---

## Token

| Property | Value |
|---|---|
| Name | Praxis |
| Symbol | $PRX |
| Denomination | μPRX (micro-PRX) |
| Chain ID | 1 |
| Network ID | 1 |

---

## License

MIT — see [LICENSE](LICENSE)
