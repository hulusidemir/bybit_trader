# Bybit Trader

A Go-based bot that scans the Bybit USDT Perpetual market across multiple timeframes, sends trading signals to Telegram, and optionally executes live trades on Bybit.

## Features

- Multi-timeframe analysis (5m / 15m / 1h / 4h)
- Confluence-based signal generation using Bybit + Binance data
- Telegram notifications (startup, signals, order fills, TP hits, closures)
- Optional live trading mode (`TRADING_ENABLED=true`)
- Built-in SQLite trade tracking (`trades.db`)
- Built-in web dashboard (`/`, `/api/trades`, `/api/stats`, `/api/active`, `/api/patterns`)

## How It Works

1. Fetches all USDT perpetual instruments from Bybit.
2. Filters out coins below `MIN_VOLUME_24H_USD` and applies the blacklist.
3. Runs the analysis engine per coin: OI, CVD, orderbook, ATR, and L/S ratio.
4. Selects high-quality signals from pattern + MTF confluence matches.
5. Sends signals to Telegram.
6. If live trading is enabled, places orders on Bybit; the monitor manages TP/DCA and order lifecycle.

## Requirements

- Go 1.22+
- Telegram Bot Token & Chat ID
- (For live trading) Bybit API Key / Secret

## Installation

```bash
git clone <repo-url>
cd bybit_trader
cp .env.example .env
```

Edit the `.env` file with your credentials:

```bash
nano .env
```

## Running

### Development

```bash
go run ./cmd/trader/
```

### Production

```bash
go build -o bybit_trader ./cmd/trader/
./bybit_trader
```

Once started:
- Dashboard: `http://localhost:8082` (default)
- Database: `trades.db`

## Configuration (.env)

### Required

- `TELEGRAM_BOT_TOKEN`
- `TELEGRAM_CHAT_ID`

### Scanning

- `SCAN_INTERVAL_SECONDS` (default: `300`)
- `MIN_VOLUME_24H_USD` (default: `10000000`)
- `DASHBOARD_PORT` (default: `8081`)

### Live Trading

- `TRADING_ENABLED` (`false` / `true`)
- `BYBIT_API_KEY`
- `BYBIT_API_SECRET`
- `BYBIT_TESTNET` (`false` / `true`)
- `LEVERAGE` (default: `10`)
- `MARGIN_PER_TRADE` (default: `100`)
- `MAX_DCA_COUNT` (default: `3`)
- `DCA_THRESHOLD_PCT` (default: `20.0`)
- `ORDER_TIMEOUT_SEC` (default: `300`)

### TP / DCA Settings

- `TP1_PCT` (default: `1.0`)
- `TP2_PCT` (default: `2.5`)
- `TP3_PCT` (default: `5.0`)
- `ATR_TP2_MULT` (default: `3.5`)
- `ATR_TP3_MULT` (default: `6.0`)

### Per-Coin Filters / Overrides

- `COIN_BLACKLIST` — e.g. `BTCUSDT,ETHUSDT`
- `COIN_MARGIN_OVERRIDES` — e.g. `BTCUSDT:500,ETHUSDT:300`
- `COIN_DCA_OVERRIDES` — e.g. `BTCUSDT:3,SOLUSDT:5`

## Operating Modes

### 1) Signal-Only Mode (`TRADING_ENABLED=false`)

- Signals are generated and sent to Telegram.
- No real orders are placed.
- Trades are recorded for tracking purposes only.

### 2) Live Trading Mode (`TRADING_ENABLED=true`)

- Limit entry orders are placed on Bybit after each signal.
- Order fills are tracked by the monitor.
- TP1 / TP2 / TP3 partial closes are managed automatically.
- DCA entries are executed via market orders when triggered.

## Dashboard API

- `GET /api/trades?filter=active|closed`
- `GET /api/stats`
- `GET /api/active`
- `GET /api/patterns`

## Running with Systemd (Linux)

See the example service file: `bybit_trader.service.example`

1. Build the binary:
   ```bash
   go build -o bybit_trader ./cmd/trader/
   ```
2. Copy and edit the service file (`User`, `WorkingDirectory`, `ExecStart`):
   ```bash
   sudo cp bybit_trader.service.example /etc/systemd/system/bybit_trader.service
   sudo nano /etc/systemd/system/bybit_trader.service
   ```
3. Enable and start the service:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable bybit_trader
   sudo systemctl start bybit_trader
   sudo systemctl status bybit_trader
   ```

## Project Structure

- `cmd/trader/main.go` — application entry point
- `pkg/analysis` — analysis engine, pattern matching, MTF logic
- `pkg/signals` — signal generation and Telegram message formatting
- `pkg/exchange/bybit` — Bybit market data + trade client
- `pkg/exchange/binance` — Binance auxiliary data sources
- `pkg/executor` — order placement, partial closes, DCA
- `pkg/tracker` — trade store (SQLite) + price monitor
- `pkg/dashboard` — web dashboard and API
- `pkg/config` — `.env` loading and validation

## Disclaimer

Cryptocurrency trading carries a high level of risk. This project is for educational and research purposes only and does not guarantee profits. Always validate with testnet and conservative settings before enabling live trading.
