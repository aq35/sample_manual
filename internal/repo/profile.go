package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Profile は対象のめったに変わらない情報（状態と同じ行に置かない。調査 §4.4）。
type Profile struct {
	RobotID   string
	Name      string
	ModelName string
	Serial    string
	Version   uint64
	UpdatedAt time.Time
}

// ProfileRepo はリポジトリの実装例。
//
// ★受け取るのは *Scope（テナントに束縛されたハンドル）だけ。*sql.DB は持たない。
// これにより「テナントを指定し忘れた SQL」が構造的に書けない。
type ProfileRepo struct{}

// Get は主キーで1件引く。
func (ProfileRepo) Get(ctx context.Context, s *Scope, robotID string) (Profile, error) {
	const q = `SELECT robot_id, name, model_name, serial, version, updated_at
	             FROM robot_profile
	            WHERE tenant_id = :tenant AND robot_id = ?`
	var p Profile
	err := s.QueryRow(ctx, "profile.Get", q, robotID).
		Scan(&p.RobotID, &p.Name, &p.ModelName, &p.Serial, &p.Version, &p.UpdatedAt)
	if err != nil {
		return Profile{}, err
	}
	return p, nil
}

// List はキーセット法の一覧。OFFSET は使わない（実験参照）。
func (ProfileRepo) List(ctx context.Context, s *Scope, ks Keyset) (Page[Profile], error) {
	const q = `SELECT robot_id, name, model_name, serial, version, updated_at
	             FROM robot_profile
	            WHERE tenant_id = :tenant AND robot_id > ?
	         ORDER BY robot_id
	            LIMIT ?`
	return Paginate(ctx, s, "profile.List", q, ks, func(rows *sql.Rows) (Profile, string, error) {
		var p Profile
		if err := rows.Scan(&p.RobotID, &p.Name, &p.ModelName, &p.Serial, &p.Version, &p.UpdatedAt); err != nil {
			return Profile{}, "", err
		}
		return p, p.RobotID, nil
	})
}

// GetMany は複数件をまとめて引く（N+1 を書かせないための入口）。
// 実験（TestExperiment_N1問題）では 500 件で 64 倍の差が出た。
func (ProfileRepo) GetMany(ctx context.Context, s *Scope, robotIDs []string) (map[string]Profile, error) {
	if len(robotIDs) == 0 {
		return map[string]Profile{}, nil
	}
	if len(robotIDs) > s.db.opt.MaxRows {
		return nil, wrap("profile.GetMany", string(s.tenant), "",
			fmt.Errorf("%w: %d 件は上限 %d 件を超えている", ErrTooManyRows, len(robotIDs), s.db.opt.MaxRows))
	}
	q := `SELECT robot_id, name, model_name, serial, version, updated_at
	        FROM robot_profile
	       WHERE tenant_id = :tenant AND robot_id IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(robotIDs)), ",") + `) LIMIT ?`

	args := make([]any, 0, len(robotIDs)+1)
	for _, id := range robotIDs {
		args = append(args, id)
	}
	args = append(args, len(robotIDs))

	rows, err := s.Query(ctx, "profile.GetMany", q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]Profile, len(robotIDs))
	for rows.Next() {
		var p Profile
		if err := rows.Scan(&p.RobotID, &p.Name, &p.ModelName, &p.Serial, &p.Version, &p.UpdatedAt); err != nil {
			return nil, wrap("profile.GetMany", string(s.tenant), q, err)
		}
		out[p.RobotID] = p
	}
	return out, wrap("profile.GetMany", string(s.tenant), q, rows.Err())
}

// Create は新規登録。すでにあれば ErrConflict。
func (ProfileRepo) Create(ctx context.Context, s *Scope, p Profile) error {
	const q = `INSERT INTO robot_profile (tenant_id, robot_id, name, model_name, serial)
	           VALUES (:tenant, ?, ?, ?, ?)`
	_, err := s.Exec(ctx, "profile.Create", q, ExpectOne, p.RobotID, p.Name, p.ModelName, p.Serial)
	return err
}

// Rename は「読んで書く」更新。★version が一致したときだけ書く（楽観ロック）。
//
// 実験（TestExperiment_更新が消える）では、version 無しの read-modify-write で
// 200 回中 174 回の更新が消えた。消えてもエラーは出ない。
func (ProfileRepo) Rename(ctx context.Context, s *Scope, robotID, name string, version uint64) error {
	const q = `UPDATE robot_profile
	              SET name = ?, version = version + 1
	            WHERE tenant_id = :tenant AND robot_id = ? AND version = ?`
	n, err := s.Exec(ctx, "profile.Rename", q, ExpectAtMostOne, name, robotID, version)
	if err != nil {
		return err
	}
	if n == 0 {
		// 行が無いのか、version が古いのかを区別して返す
		if _, getErr := (ProfileRepo{}).Get(ctx, s, robotID); getErr != nil {
			return getErr
		}
		return wrap("profile.Rename", string(s.tenant), q, ErrOptimisticLock)
	}
	return nil
}

// Delete は1件削除。ちょうど1行に当たらなければロールバックする。
func (ProfileRepo) Delete(ctx context.Context, s *Scope, robotID string) error {
	const q = `DELETE FROM robot_profile WHERE tenant_id = :tenant AND robot_id = ?`
	_, err := s.Exec(ctx, "profile.Delete", q, ExpectOne, robotID)
	if errors.Is(err, ErrUnexpectedRowCount) {
		return fmt.Errorf("%w: robot_id=%s", ErrNotFound, robotID)
	}
	return err
}

// Count はテナント内の件数。
func (ProfileRepo) Count(ctx context.Context, s *Scope) (int64, error) {
	const q = `SELECT COUNT(*) FROM robot_profile WHERE tenant_id = :tenant`
	var n int64
	if err := s.QueryRow(ctx, "profile.Count", q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
