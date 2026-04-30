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
[![Status](https://img.shields.io/badge/Status-V2_Active-ffc940)](ROADMAP.md)

</div>

---

## What is Praxis?

Praxis ($PRX) is a sovereign prediction market protocol built as a Canopy Nested Chain. It lets anyone create YES/NO prediction markets, stake on outcomes, and claim proportional winnings — entirely on-chain, with no platform extraction and no central authority.

Implemented as a Go plugin on Canopy Network, Praxis runs as an application-specific blockchain with its own state, its own token, and its own transaction types. It settles security on Canopy's root chain via NestBFT consensus.

> **V1 is live on Betanet. V2 development is active — see [ROADMAP.md](ROADMAP.md).**

---

## Architecture

```
Canopy Root Chain (security + consensus)
        │
        └── Praxis Nested Chain (plugin/go)
                ├── Genesis / BeginBlock / EndBlock
                ├── CheckTx + DeliverTx (4 custom tx types)
                ├── State (key-value store)
                └── Frontend (single HTML file, BLS12-381 signing)
```

---

## Transaction Types

| Transaction | Description |
|---|---|
| `create_market` | Open a YES/NO prediction market with a question, resolver, and resolution height |
| `submit_prediction` | Stake tokens on YES or NO for an open market |
| `resolve_market` | Designated resolver finalises the market with the winning outcome |
| `claim_winnings` | Winners claim proportional payout from the resolved pool |

---

## Repository Layout

```
plugin/go/
├── main.go              # Entry point
├── chain.json           # Chain config (name: Praxis, symbol: PRX)
├── pluginctl.sh         # Plugin lifecycle management
├── contract/
│   ├── contract.go      # All transaction handlers (CheckTx + DeliverTx)
│   ├── error.go         # Error codes
│   ├── plugin.go        # Socket protocol (do not modify)
│   └── tx.pb.go         # Generated protobuf types
└── proto/
    ├── tx.proto         # Message definitions
    ├── account.proto    # Account/Pool types
    └── plugin.proto     # FSM protocol

frontend/
└── index.html           # Single-file UI, no build step
```

---

## Payout Model

```
payout = stake + (stake × losing_pool) / winning_pool

Example:
  YES pool: 600,000 μPRX (forecaster contributed 200,000)
  NO pool:  400,000 μPRX

  Share: 200,000 / 600,000 = 33.3%
  Payout: 200,000 + (400,000 × 33.3%) = 333,333 μPRX
```

---

## Getting Started

```bash
git clone https://github.com/Makaveli912/canopy.git
cd canopy && git checkout feat/praxis-prediction-markets

# Build Canopy node
go build -o ~/go/bin/canopy ./cmd/main

# Build Praxis plugin
cd plugin/go
GOTOOLCHAIN=local go mod tidy
go build -o go-plugin .
cp go-plugin ~/canopy/plugin/go/go-plugin

# Start
canopy start

# Serve frontend (new tab)
cd frontend && python3 -m http.server 8080
```

---

## Token

| Property | Value |
|---|---|
| Symbol | $PRX |
| Denomination | μPRX |
| Chain ID | 1 |
| Network ID | 1 |

---

## Roadmap

V2 is actively in development. See [ROADMAP.md](ROADMAP.md) — 19 items across 3 waves including dispute system, protocol revenue (losing pool cut + volume fee), position withdrawal, leaderboard, and more.

---

## Competitive Position

| Protocol | Sovereign Chain | No Oracle | Position Exit | Dispute | Revenue |
|---|---|---|---|---|---|
| **Praxis v1** | ✓ | ✓ | ✗ | ✗ | Fees only |
| Polymarket | ✗ | ✗ | ✓ | ✓ | 2% volume |
| Augur | ✗ | ✗ | ✓ | ✓ | Reporter fees |
| **Praxis v2** | ✓ | ✓ | ✓ | ✓ | Pool cut + volume |

---

## Built By

[@MakDaVeli](https://twitter.com/MakDaVeli) 

## License

MIT


