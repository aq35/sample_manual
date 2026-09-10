// Package pklab は EXP-54（主キー設計：連番 BIGINT vs ランダム UUID）の実験本体。
//
// InnoDB のテーブルは主キーの B-tree そのもの（clustered index）。主キーが単調増加（連番 BIGINT）
// なら挿入は木の右端に追記され、ページ分割がほぼ起きず密に詰まる。ランダム（UUID v4）だと挿入位置
// が散らばり、ページ分割・断片化で index が肥大し挿入も重い。さらに二次索引は主キーを内包するので、
// 太い主キー（CHAR(36)）は全二次索引を膨らませる。時刻順 UUID なら追記に戻せる。
package pklab

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// Kind は主キーの種類。
type Kind int

const (
	BigintAuto     Kind = iota // BIGINT AUTO_INCREMENT（単調増加・追記）
	UUIDCharV4                 // CHAR(36) ランダム UUID（散らばる・太い）
	UUIDBinV4                  // BINARY(16) ランダム UUID（散らばる・細い）
	UUIDBinOrdered             // BINARY(16) 時刻順 UUID（追記に戻す）
)

func (k Kind) Table() string {
	switch k {
	case BigintAuto:
		return "pk_bigint"
	case UUIDCharV4:
		return "pk_uuid_char"
	case UUIDBinV4:
		return "pk_uuid_bin"
	case UUIDBinOrdered:
		return "pk_uuid_ord"
	}
	return "pk_unknown"
}

// Setup は4種の表を作る。payload と二次索引 k_tenant は共通（主キーの違いだけを見る）。
func Setup(ctx context.Context, db *sql.DB) error {
	defs := map[Kind]string{
		BigintAuto: `CREATE TABLE pk_bigint (
		   id BIGINT NOT NULL AUTO_INCREMENT,
		   tenant VARCHAR(32) NOT NULL, payload VARCHAR(100) NOT NULL,
		   PRIMARY KEY (id), KEY k_tenant (tenant)
		 ) ENGINE=InnoDB ROW_FORMAT=DYNAMIC`,
		UUIDCharV4: `CREATE TABLE pk_uuid_char (
		   id CHAR(36) NOT NULL,
		   tenant VARCHAR(32) NOT NULL, payload VARCHAR(100) NOT NULL,
		   PRIMARY KEY (id), KEY k_tenant (tenant)
		 ) ENGINE=InnoDB ROW_FORMAT=DYNAMIC`,
		UUIDBinV4: `CREATE TABLE pk_uuid_bin (
		   id BINARY(16) NOT NULL,
		   tenant VARCHAR(32) NOT NULL, payload VARCHAR(100) NOT NULL,
		   PRIMARY KEY (id), KEY k_tenant (tenant)
		 ) ENGINE=InnoDB ROW_FORMAT=DYNAMIC`,
		UUIDBinOrdered: `CREATE TABLE pk_uuid_ord (
		   id BINARY(16) NOT NULL,
		   tenant VARCHAR(32) NOT NULL, payload VARCHAR(100) NOT NULL,
		   PRIMARY KEY (id), KEY k_tenant (tenant)
		 ) ENGINE=InnoDB ROW_FORMAT=DYNAMIC`,
	}
	for k, def := range defs {
		if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+k.Table()); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, def); err != nil {
			return fmt.Errorf("pk setup %s: %w", k.Table(), err)
		}
	}
	return nil
}

var ordCounter uint64

// randUUIDv4 はランダム 16 バイト（v4 相当・散らばる）。
func randUUIDv4() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b
}

// orderedUUID は先頭 8 バイトを単調増加（時刻＋連番）にした 16 バイト（追記に戻す）。
func orderedUUID() []byte {
	b := make([]byte, 16)
	hi := uint64(time.Now().UnixNano())<<12 | (atomic.AddUint64(&ordCounter, 1) & 0xfff)
	binary.BigEndian.PutUint64(b[0:8], hi)
	_, _ = rand.Read(b[8:16])
	return b
}

func hyphenate(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Insert は n 行を chunk 件ずつの multi-row INSERT で入れる（往復は全種で同じ。主キーの順序だけが違う）。
// 返り値は所要時間。
func Insert(ctx context.Context, db *sql.DB, k Kind, tenant string, n, chunk int) (time.Duration, error) {
	pad := strings.Repeat("p", 80)
	t0 := time.Now()
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		var b strings.Builder
		var args []any
		switch k {
		case BigintAuto:
			b.WriteString("INSERT INTO pk_bigint (tenant, payload) VALUES ")
			for i := start; i < end; i++ {
				if i > start {
					b.WriteString(",")
				}
				b.WriteString("(?,?)")
				args = append(args, tenant, pad)
			}
		default:
			b.WriteString("INSERT INTO " + k.Table() + " (id, tenant, payload) VALUES ")
			for i := start; i < end; i++ {
				if i > start {
					b.WriteString(",")
				}
				b.WriteString("(?,?,?)")
				var id any
				//smlint:allow exhaustive 理由: BigintAuto は外側 switch の case で処理済み。ここは default 側で到達しない
				switch k {
				case UUIDCharV4:
					id = hyphenate(randUUIDv4())
				case UUIDBinV4:
					id = randUUIDv4()
				case UUIDBinOrdered:
					id = orderedUUID()
				}
				args = append(args, id, tenant, pad)
			}
		}
		//smlint:allow loopquery 理由: これが測定対象の一括 INSERT。chunk 件ずつまとめている
		if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
			return 0, err
		}
	}
	return time.Since(t0), nil
}

// Sizes は ANALYZE 後の data_length / index_length（バイト）を返す。
func Sizes(ctx context.Context, db *sql.DB, k Kind) (dataLen, indexLen int64, err error) {
	if _, err = db.ExecContext(ctx, "ANALYZE TABLE "+k.Table()); err != nil {
		return 0, 0, err
	}
	err = db.QueryRowContext(ctx,
		"SELECT data_length, index_length FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?",
		k.Table()).Scan(&dataLen, &indexLen)
	return dataLen, indexLen, err
}
