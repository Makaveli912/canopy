# Praxis — Prediction Market on Canopy

A fully on-chain YES/NO prediction market built as a Canopy appchain plugin for the Vibe Coding Contest.

## What it does
Users create prediction markets with a YES/NO question and a resolution block height.
Other users place bets with $PRX tokens. When the resolution height is reached,
the designated resolver declares the winner. Correct forecasters claim a proportional
payout from the losing pool.

## Transaction types
- `create_market` — open a new prediction market
- `submit_prediction` — bet YES or NO on a market
- `resolve_market` — resolver declares the winner
- `claim_winnings` — winner collects payout

## How to run locally
1. Drop `plugin/go/proto/tx.proto` and `plugin/go/contract/contract.go` into your Canopy repo
2. Run `cd plugin/go/proto && ./_generate.sh` to regenerate Go code
3. Run `cd plugin/go && make build` to build the plugin
4. Set `"plugin": "go"` in `~/.canopy/config.json`
5. Run `canopy start` — look for `plugin connected: praxis_prediction_market`
6. Open `plugin/go/frontend/index.html` in your browser

## RPC ports
- Port 50002 — public RPC (queries + transaction submission)
- Port 50003 — admin RPC (keystore, key management)

## Payout formula
`payout = your_stake + (your_stake × losing_pool / winning_pool)`
