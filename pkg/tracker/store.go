package tracker

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"bybit_trader/pkg/models"
)

type Store struct {
	db *sql.DB
}

func NewStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := createTables(db); err != nil {
		return nil, fmt.Errorf("create tables: %w", err)
	}

	if err := ensureTradeColumns(db); err != nil {
		return nil, fmt.Errorf("ensure trade columns: %w", err)
	}

	return &Store{db: db}, nil
}

func createTables(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS trades (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			signal_id TEXT UNIQUE NOT NULL,
			symbol TEXT NOT NULL,
			direction TEXT NOT NULL,
			pattern TEXT NOT NULL,
			grade TEXT NOT NULL,
			entry_price REAL NOT NULL,
			stop_loss REAL NOT NULL,
			tp1 REAL NOT NULL,
			tp2 REAL NOT NULL,
			tp3 REAL NOT NULL,
			exit_price REAL DEFAULT 0,
			status TEXT DEFAULT 'PENDING',
			pnl_percent REAL DEFAULT 0,
			current_price REAL DEFAULT 0,
			order_id TEXT DEFAULT '',
			avg_entry_price REAL DEFAULT 0,
			total_qty REAL DEFAULT 0,
			remaining_qty REAL DEFAULT 0,
			dca_count INTEGER DEFAULT 0,
			margin_used REAL DEFAULT 0,
			margin_per_entry REAL DEFAULT 0,
			last_dca_price REAL DEFAULT 0,
			last_dca_time DATETIME,
			tp1_order_id TEXT DEFAULT '',
			tp2_order_id TEXT DEFAULT '',
			tp3_order_id TEXT DEFAULT '',
			tp_phase TEXT DEFAULT '',
			opened_at DATETIME NOT NULL,
			closed_at DATETIME,
			moved_to_tp1_at DATETIME,
			moved_to_tp2_at DATETIME
		);
		CREATE INDEX IF NOT EXISTS idx_trades_status ON trades(status);
		CREATE INDEX IF NOT EXISTS idx_trades_symbol ON trades(symbol);
	`)
	return err
}

func ensureTradeColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(trades)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	existing := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		existing[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if !existing["moved_to_tp1_at"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN moved_to_tp1_at DATETIME`); err != nil {
			return err
		}
	}
	if !existing["moved_to_tp2_at"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN moved_to_tp2_at DATETIME`); err != nil {
			return err
		}
	}
	if !existing["order_id"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN order_id TEXT DEFAULT ''`); err != nil {
			return err
		}
	}
	if !existing["avg_entry_price"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN avg_entry_price REAL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !existing["total_qty"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN total_qty REAL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !existing["remaining_qty"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN remaining_qty REAL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !existing["dca_count"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN dca_count INTEGER DEFAULT 0`); err != nil {
			return err
		}
	}
	if !existing["margin_used"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN margin_used REAL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !existing["margin_per_entry"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN margin_per_entry REAL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !existing["last_dca_price"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN last_dca_price REAL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !existing["last_dca_time"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN last_dca_time DATETIME`); err != nil {
			return err
		}
	}
	if !existing["tp1_order_id"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN tp1_order_id TEXT DEFAULT ''`); err != nil {
			return err
		}
	}
	if !existing["tp2_order_id"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN tp2_order_id TEXT DEFAULT ''`); err != nil {
			return err
		}
	}
	if !existing["tp3_order_id"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN tp3_order_id TEXT DEFAULT ''`); err != nil {
			return err
		}
	}
	if !existing["tp_phase"] {
		if _, err := db.Exec(`ALTER TABLE trades ADD COLUMN tp_phase TEXT DEFAULT ''`); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) CreateTrade(sig *models.Signal, orderID string, entryPrice, qty, margin float64) (*models.Trade, error) {
	now := time.Now()

	result, err := s.db.Exec(`
		INSERT INTO trades (signal_id, symbol, direction, pattern, grade,
			entry_price, stop_loss, tp1, tp2, tp3, status, current_price,
			order_id, avg_entry_price, total_qty, remaining_qty, dca_count,
			margin_used, margin_per_entry, last_dca_price,
			opened_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'PENDING', ?, ?, ?, ?, ?, 0, ?, ?, 0, ?)
	`, sig.ID, sig.Symbol, sig.Direction, sig.Pattern, sig.Grade,
		entryPrice, sig.StopLoss, sig.TP1, sig.TP2, sig.TP3, entryPrice,
		orderID, entryPrice, qty, qty, margin, margin, now)
	if err != nil {
		return nil, fmt.Errorf("insert trade: %w", err)
	}

	id, _ := result.LastInsertId()
	return &models.Trade{
		ID:             id,
		SignalID:       sig.ID,
		Symbol:         sig.Symbol,
		Direction:      sig.Direction,
		Pattern:        sig.Pattern,
		Grade:          sig.Grade,
		EntryPrice:     entryPrice,
		StopLoss:       sig.StopLoss,
		TP1:            sig.TP1,
		TP2:            sig.TP2,
		TP3:            sig.TP3,
		Status:         models.TradePending,
		OrderID:        orderID,
		AvgEntryPrice:  entryPrice,
		TotalQty:       qty,
		RemainingQty:   qty,
		DCACount:       0,
		MarginUsed:     margin,
		MarginPerEntry: margin,
		OpenedAt:       now,
	}, nil
}

func (s *Store) UpdateTrade(id int64, status models.TradeStatus, exitPrice, pnl float64) error {
	now := time.Now()
	_, err := s.db.Exec(`
		UPDATE trades SET status = ?, exit_price = ?, pnl_percent = ?, closed_at = ?
		WHERE id = ?
	`, status, exitPrice, pnl, now, id)
	return err
}

func (s *Store) UpdateCurrentPrice(id int64, price float64) error {
	_, err := s.db.Exec(`UPDATE trades SET current_price = ? WHERE id = ?`, price, id)
	return err
}

func (s *Store) UpdateStopLoss(id int64, stopLoss float64) error {
	_, err := s.db.Exec(`UPDATE trades SET stop_loss = ? WHERE id = ?`, stopLoss, id)
	return err
}

func (s *Store) MarkStopMoved(id int64, level string, at time.Time) error {
	switch level {
	case "TP1":
		_, err := s.db.Exec(`UPDATE trades SET moved_to_tp1_at = ? WHERE id = ?`, at, id)
		return err
	case "TP2":
		_, err := s.db.Exec(`UPDATE trades SET moved_to_tp2_at = ? WHERE id = ?`, at, id)
		return err
	default:
		return nil
	}
}

func (s *Store) GetActiveTrades() ([]*models.Trade, error) {
	return s.queryTrades("WHERE status = 'ACTIVE'")
}

func (s *Store) GetAllTrades() ([]*models.Trade, error) {
	return s.queryTrades("ORDER BY opened_at DESC LIMIT 200")
}

func (s *Store) GetClosedTrades() ([]*models.Trade, error) {
	return s.queryTrades("WHERE status != 'ACTIVE' ORDER BY closed_at DESC LIMIT 200")
}

func (s *Store) queryTrades(where string) ([]*models.Trade, error) {
	rows, err := s.db.Query(`
		SELECT id, signal_id, symbol, direction, pattern, grade,
			entry_price, stop_loss, tp1, tp2, tp3,
			exit_price, status, pnl_percent, current_price,
			COALESCE(order_id, ''), COALESCE(avg_entry_price, entry_price),
			COALESCE(total_qty, 0), COALESCE(remaining_qty, 0),
			COALESCE(dca_count, 0), COALESCE(margin_used, 0),
			COALESCE(margin_per_entry, margin_used), COALESCE(last_dca_price, 0),
			last_dca_time,
			COALESCE(tp1_order_id, ''), COALESCE(tp2_order_id, ''), COALESCE(tp3_order_id, ''),
			COALESCE(tp_phase, ''),
			opened_at, closed_at, moved_to_tp1_at, moved_to_tp2_at
		FROM trades ` + where)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var trades []*models.Trade
	for rows.Next() {
		t := &models.Trade{}
		var closedAt sql.NullTime
		var movedTP1At sql.NullTime
		var movedTP2At sql.NullTime
		var lastDCATime sql.NullTime
		var dir, pattern, grade, status string

		var tpPhase string
		err := rows.Scan(
			&t.ID, &t.SignalID, &t.Symbol, &dir, &pattern, &grade,
			&t.EntryPrice, &t.StopLoss, &t.TP1, &t.TP2, &t.TP3,
			&t.ExitPrice, &status, &t.PnLPercent, &t.CurrentPrice,
			&t.OrderID, &t.AvgEntryPrice,
			&t.TotalQty, &t.RemainingQty,
			&t.DCACount, &t.MarginUsed, &t.MarginPerEntry, &t.LastDCAPrice,
			&lastDCATime,
			&t.TP1OrderID, &t.TP2OrderID, &t.TP3OrderID,
			&tpPhase,
			&t.OpenedAt, &closedAt, &movedTP1At, &movedTP2At,
		)
		if err != nil {
			return nil, err
		}

		t.Direction = models.SignalDirection(dir)
		t.Pattern = models.PatternName(pattern)
		t.Grade = models.SignalGrade(grade)
		t.Status = models.TradeStatus(status)
		t.TPPhase = models.TPPhase(tpPhase)
		if closedAt.Valid {
			t.ClosedAt = &closedAt.Time
		}
		if movedTP1At.Valid {
			t.MovedToTP1At = &movedTP1At.Time
		}
		if movedTP2At.Valid {
			t.MovedToTP2At = &movedTP2At.Time
		}
		if lastDCATime.Valid {
			t.LastDCATime = lastDCATime.Time
		}

		trades = append(trades, t)
	}

	return trades, rows.Err()
}

func (s *Store) GetStats() (*models.TradeStats, error) {
	trades, err := s.GetAllTrades()
	if err != nil {
		return nil, err
	}

	stats := &models.TradeStats{
		PatternStats: make(map[models.PatternName]*models.PatternStat),
	}

	for _, t := range trades {
		stats.TotalTrades++

		if t.Status == models.TradeActive {
			stats.ActiveTrades++
			stats.TotalMargin += t.MarginUsed
			continue
		}

		// Skip cancelled/pending orders — they never filled, not real trades
		if t.Status == models.TradeCancelled || t.Status == models.TradePending {
			stats.CancelledTrades++
			continue
		}

		if t.PnLPercent > 0 {
			stats.WinTrades++
			stats.TotalPnL += t.PnLPercent
			if t.PnLPercent > stats.BestTrade {
				stats.BestTrade = t.PnLPercent
			}
		} else {
			stats.LossTrades++
			stats.TotalPnL += t.PnLPercent
			if t.PnLPercent < stats.WorstTrade {
				stats.WorstTrade = t.PnLPercent
			}
		}

		switch t.Status {
		case models.TradeTP1:
			stats.TP1Count++
		case models.TradeTP2:
			stats.TP2Count++
		case models.TradeTP3:
			stats.TP3Count++
		}

		// Pattern stats
		ps, ok := stats.PatternStats[t.Pattern]
		if !ok {
			ps = &models.PatternStat{Name: t.Pattern}
			stats.PatternStats[t.Pattern] = ps
		}
		ps.Total++
		if t.PnLPercent > 0 {
			ps.Wins++
		} else {
			ps.Losses++
		}
		ps.AvgPnL = (ps.AvgPnL*float64(ps.Total-1) + t.PnLPercent) / float64(ps.Total)
	}

	closed := stats.WinTrades + stats.LossTrades
	if closed > 0 {
		stats.WinRate = float64(stats.WinTrades) / float64(closed) * 100
		if stats.WinTrades > 0 {
			stats.AvgWin = stats.TotalPnL / float64(stats.WinTrades)
		}
		if stats.LossTrades > 0 {
			// avgLoss calculation using only losses
			totalLoss := 0.0
			for _, t := range trades {
				if t.PnLPercent < 0 {
					totalLoss += t.PnLPercent
				}
			}
			stats.AvgLoss = totalLoss / float64(stats.LossTrades)
		}
	}

	for _, ps := range stats.PatternStats {
		if ps.Total > 0 {
			ps.WinRate = float64(ps.Wins) / float64(ps.Total) * 100
		}
	}

	return stats, nil
}

// GetPendingTrades returns trades waiting for order fill
func (s *Store) GetPendingTrades() ([]*models.Trade, error) {
	return s.queryTrades("WHERE status = 'PENDING'")
}

// GetActiveTradeBySymbol returns the active/pending trade for a symbol (if any)
func (s *Store) GetActiveTradeBySymbol(symbol string) (*models.Trade, error) {
	trades, err := s.queryTrades(fmt.Sprintf(
		"WHERE symbol = '%s' AND status IN ('ACTIVE', 'PENDING') LIMIT 1", symbol))
	if err != nil {
		return nil, err
	}
	if len(trades) == 0 {
		return nil, nil
	}
	return trades[0], nil
}

// ActivateTrade marks a pending trade as active after order fill
func (s *Store) ActivateTrade(id int64, avgPrice, filledQty float64) error {
	_, err := s.db.Exec(`
		UPDATE trades SET status = 'ACTIVE', avg_entry_price = ?, total_qty = ?, remaining_qty = ?, entry_price = ?
		WHERE id = ?
	`, avgPrice, filledQty, filledQty, avgPrice, id)
	return err
}

// CancelTrade marks a pending trade as cancelled
func (s *Store) CancelTrade(id int64) error {
	now := time.Now()
	_, err := s.db.Exec(`
		UPDATE trades SET status = 'CANCELLED', closed_at = ?
		WHERE id = ?
	`, now, id)
	return err
}

// UpdateDCA updates trade after a DCA entry
func (s *Store) UpdateDCA(id int64, newAvgEntry, newTotalQty, newRemainingQty float64,
	dcaCount int, marginUsed, lastDCAPrice, tp1, tp2, tp3 float64) error {
	now := time.Now()
	_, err := s.db.Exec(`
		UPDATE trades SET
			avg_entry_price = ?, total_qty = ?, remaining_qty = ?,
			dca_count = ?, margin_used = ?, last_dca_price = ?,
			last_dca_time = ?,
			tp1 = ?, tp2 = ?, tp3 = ?
		WHERE id = ?
	`, newAvgEntry, newTotalQty, newRemainingQty,
		dcaCount, marginUsed, lastDCAPrice,
		now,
		tp1, tp2, tp3, id)
	return err
}

// UpdateRemainingQty updates remaining qty after partial TP close
func (s *Store) UpdateRemainingQty(id int64, remainingQty float64) error {
	_, err := s.db.Exec(`UPDATE trades SET remaining_qty = ? WHERE id = ?`, remainingQty, id)
	return err
}

// UpdateTPOrder stores a TP limit order ID and updates the TP phase
func (s *Store) UpdateTPOrder(id int64, phase models.TPPhase, tp1OrderID, tp2OrderID, tp3OrderID string) error {
	_, err := s.db.Exec(`
		UPDATE trades SET tp_phase = ?, tp1_order_id = ?, tp2_order_id = ?, tp3_order_id = ?
		WHERE id = ?
	`, phase, tp1OrderID, tp2OrderID, tp3OrderID, id)
	return err
}

// UpdateTPPhase updates only the TP phase
func (s *Store) UpdateTPPhase(id int64, phase models.TPPhase) error {
	_, err := s.db.Exec(`UPDATE trades SET tp_phase = ? WHERE id = ?`, phase, id)
	return err
}

// UpdateTradeStatus updates status and TP timestamps
func (s *Store) UpdateTradeStatus(id int64, status models.TradeStatus) error {
	now := time.Now()
	switch status {
	case models.TradeTP1:
		_, err := s.db.Exec(`UPDATE trades SET status = ?, moved_to_tp1_at = ? WHERE id = ?`, status, now, id)
		return err
	case models.TradeTP2:
		_, err := s.db.Exec(`UPDATE trades SET status = ?, moved_to_tp2_at = ? WHERE id = ?`, status, now, id)
		return err
	default:
		_, err := s.db.Exec(`UPDATE trades SET status = ? WHERE id = ?`, status, id)
		return err
	}
}

func (s *Store) Close() error {
	return s.db.Close()
}
