package store

import (
	"context"

	"github.com/KangminNa/naru/internal/model"
)

// DomainMoves는 한 서비스의 주소와 웹서버 설정을 다른 서비스로 옮긴다 (DomainMover).
type DomainMoves struct{ db *DB }

func NewDomainMoves(db *DB) DomainMoves { return DomainMoves{db} }

// Move는 from의 주소 전부와 웹서버 설정을 to로 옮기고, to의 "넘겨받을 서비스" 표시를 지운다 — 한 트랜잭션.
// from이 이미 없으면 옮길 것이 없고 표시만 지운다. from에 웹서버 설정이 없으면 to의 것을 그대로 둔다.
func (m DomainMoves) Move(ctx context.Context, from, to model.ServiceID) (int, error) {
	tx, err := m.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE domains SET service_id = ? WHERE service_id = ?`, int64(to), int64(from))
	if err != nil {
		return 0, err
	}
	moved, _ := res.RowsAffected()
	var has int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM web_settings WHERE service_id = ?`, int64(from)).Scan(&has); err != nil {
		return 0, err
	}
	if has > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM web_settings WHERE service_id = ?`, int64(to)); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE web_settings SET service_id = ? WHERE service_id = ?`, int64(to), int64(from)); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE services SET takes_over = 0 WHERE id = ?`, int64(to)); err != nil {
		return 0, err
	}
	return int(moved), tx.Commit()
}
