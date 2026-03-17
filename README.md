# Bybit Trader

Bybit USDT Perpetual piyasasını çoklu zaman diliminde tarayan, Telegram’a sinyal gönderen ve isteğe bağlı olarak Bybit üzerinde otomatik işlem açabilen Go tabanlı bir bottur.

## Öne Çıkanlar

- 5m / 15m / 1h / 4h timeframe analizi
- Bybit + Binance verisi ile confluence tabanlı sinyal üretimi
- Telegram bildirimleri (başlangıç, sinyal, emir dolumu, TP, kapanış)
- İsteğe bağlı canlı işlem modu (`TRADING_ENABLED=true`)
- Dahili SQLite trade takibi (`trades.db`)
- Dahili dashboard (`/`, `/api/trades`, `/api/stats`, `/api/active`, `/api/patterns`)

## Nasıl Çalışır?

1. Bot Bybit enstrümanlarını çeker.
2. `MIN_VOLUME_24H_USD` altındaki coinleri eler, blacklist’i uygular.
3. Her coin için analiz motoru OI, CVD, orderbook, ATR ve L/S oranı üretir.
4. Pattern + MTF eşleşmelerinden kaliteli sinyaller seçilir.
5. Sinyaller Telegram’a gönderilir.
6. Canlı işlem açıksa Bybit’e emir gönderilir; monitor TP/DCA ve emir durumlarını yönetir.

## Gereksinimler

- Go 1.22+
- Telegram Bot Token ve Chat ID
- (Canlı işlem için) Bybit API Key/Secret

## Kurulum

```bash
git clone <repo-url>
cd bybit_trader
cp .env.example .env
```

`.env` dosyasını düzenleyin:

```bash
nano .env
```

## Çalıştırma

### Geliştirme (go run)

```bash
go run ./cmd/scanner/
```

### Üretim (binary)

```bash
go build -o bybit_trader ./cmd/scanner/
./bybit_trader
```

Bot açılınca:
- Dashboard varsayılan: `http://localhost:8082`
- SQLite dosyası: `trades.db`

## Konfigürasyon (.env)

### Zorunlu

- `TELEGRAM_BOT_TOKEN`
- `TELEGRAM_CHAT_ID`

### Tarama

- `SCAN_INTERVAL_SECONDS` (varsayılan: `300`)
- `MIN_VOLUME_24H_USD` (varsayılan: `10000000`)
- `DASHBOARD_PORT` (varsayılan: `8081`)

### Canlı işlem

- `TRADING_ENABLED` (`false`/`true`)
- `BYBIT_API_KEY`
- `BYBIT_API_SECRET`
- `BYBIT_TESTNET` (`false`/`true`)
- `LEVERAGE` (varsayılan: `10`)
- `MARGIN_PER_TRADE` (varsayılan: `100`)
- `MAX_DCA_COUNT` (varsayılan: `3`)
- `DCA_THRESHOLD_PCT` (varsayılan: `20.0`)
- `ORDER_TIMEOUT_SEC` (varsayılan: `300`)

### TP/DCA ayarları

- `TP1_PCT` (varsayılan: `1.0`)
- `TP2_PCT` (varsayılan: `2.5`)
- `TP3_PCT` (varsayılan: `5.0`)
- `ATR_TP2_MULT` (varsayılan: `3.5`)
- `ATR_TP3_MULT` (varsayılan: `6.0`)

### Coin bazlı filtre/override

- `COIN_BLACKLIST` → `BTCUSDT,ETHUSDT`
- `COIN_MARGIN_OVERRIDES` → `BTCUSDT:500,ETHUSDT:300`
- `COIN_DCA_OVERRIDES` → `BTCUSDT:3,SOLUSDT:5`

## Çalışma Modları

### 1) Sinyal modu (`TRADING_ENABLED=false`)

- Sinyaller üretilir ve Telegram’a gider.
- Gerçek emir gönderilmez.
- Trade kayıtları takip amaçlı tutulur.

### 2) Canlı işlem modu (`TRADING_ENABLED=true`)

- Sinyal sonrası Bybit’e limit giriş emri atılır.
- Emir dolumu monitor tarafından izlenir.
- TP1/TP2/TP3 kademeli kapanış yönetilir.
- DCA tetiklenirse market emir ile ekleme yapılır.

## Dashboard API

- `GET /api/trades?filter=active|closed`
- `GET /api/stats`
- `GET /api/active`
- `GET /api/patterns`

## Systemd ile Çalıştırma (Linux)

Repodaki örnek dosya: `bybit_trader.service.example`

1. Binary oluşturun:
   ```bash
   go build -o bybit_trader ./cmd/scanner/
   ```
2. Servis dosyasını kopyalayın ve düzenleyin (`User`, `WorkingDirectory`, `ExecStart`):
   ```bash
   sudo cp bybit_trader.service.example /etc/systemd/system/bybit_trader.service
   sudo nano /etc/systemd/system/bybit_trader.service
   ```
3. Servisi etkinleştirip başlatın:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable bybit_trader
   sudo systemctl start bybit_trader
   sudo systemctl status bybit_trader
   ```

## Proje Yapısı

- `cmd/scanner/main.go` → uygulama giriş noktası
- `pkg/analysis` → analiz motoru ve pattern/MTF logic
- `pkg/signals` → sinyal üretimi ve mesaj formatlama
- `pkg/exchange/bybit` → Bybit market + trade client
- `pkg/exchange/binance` → Binance yardımcı veri kaynakları
- `pkg/executor` → emir açma/kapatma/DCA
- `pkg/tracker` → trade store + monitor
- `pkg/dashboard` → web dashboard
- `pkg/config` → `.env` yükleme ve doğrulama

## Uyarı

Kripto işlemler yüksek risk içerir. Bu proje eğitim/araştırma amaçlıdır; kâr garantisi vermez. Canlı işlem modunu kullanmadan önce testnet ve düşük riskli ayarlarla doğrulama yapın.
