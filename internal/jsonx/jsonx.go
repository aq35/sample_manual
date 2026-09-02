// Package jsonx は §6「JSON の扱い」をそのまま実行できる形にしたもの。
//
//   - 要る項目だけの構造体に展開する（map[string]any は使わない）
//   - 大きい配列は Decoder で1件ずつ読む
//   - 時刻は文字列で受けてから自分で変換する
//   - DisallowUnknownFields は使わない。知らない項目は「記録して続ける」
package jsonx

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aq35/sample_manual/internal/model"
)

// SlimRobot は「要る項目だけ」書いた構造体（§6.1）。
//
// encoding/json は構造体に無い項目を読み飛ばすので、API が20項目返しても
// 書かなければメモリに載らない。「将来使うかもしれないから全部定義しておく」は
// そのぶん毎回払うことになる。
type SlimRobot struct {
	ID     string  `json:"id"`
	Status string  `json:"status"`
	Online bool    `json:"online"`
	Batt   float64 `json:"battery"`
	// ★time.Time ではなく string で受ける（§6.3）。
	// time.Time に直接読ませると、1件の書式ずれで全件が落ちる。
	ObservedAt string `json:"observed_at"`
}

// FatRobot は「API が返す項目を全部書いた」構造体。§6.1 の比較対象で、
// 実装として推奨しているわけではない（メモリ 8.3倍の差を測るために置いてある）。
type FatRobot struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	Online       bool    `json:"online"`
	Batt         float64 `json:"battery"`
	ObservedAt   string  `json:"observed_at"`
	Name         string  `json:"name"`
	Model        string  `json:"model"`
	Firmware     string  `json:"firmware"`
	Serial       string  `json:"serial"`
	Site         string  `json:"site"`
	Zone         string  `json:"zone"`
	IPAddress    string  `json:"ip_address"`
	MACAddress   string  `json:"mac_address"`
	Uptime       int64   `json:"uptime_sec"`
	ErrorCode    string  `json:"error_code"`
	ErrorMessage string  `json:"error_message"`
	TaskID       string  `json:"task_id"`
	TaskProgress float64 `json:"task_progress"`
	Temperature  float64 `json:"temperature"`
	Humidity     float64 `json:"humidity"`
	Odometer     float64 `json:"odometer_m"`
	LastMaint    string  `json:"last_maintenance_at"`
}

// SlimResponse / FatResponse は一覧 API の応答。
type SlimResponse struct {
	Robots []SlimRobot `json:"robots"`
}

type FatResponse struct {
	Robots []FatRobot `json:"robots"`
}

// ToObservation は受け取った1件を、比較用の値に変換する。
// 変換の失敗は「その1件を捨てる」で済ませ、全体を落とさない（§6.3）。
func (r SlimRobot) ToObservation(src model.Source, battStep int) (model.ID, model.Observation, error) {
	at, err := time.Parse(time.RFC3339, r.ObservedAt)
	if err != nil {
		return "", model.Observation{}, fmt.Errorf("observed_at %q: %w", r.ObservedAt, err)
	}
	st, ok := model.ParseStatus(r.Status)
	if !ok {
		// 知らない status は「不明」にして続ける。ここで落とさない（§6.3）。
		st = model.StatusUnknown
	}
	return model.ID(r.ID), model.Observation{
		State: model.State{
			Status:  st,
			Online:  r.Online,
			Battery: model.RoundBattery(r.Batt, battStep),
		},
		ObservedAt: at,
		Source:     src,
	}, nil
}

// StreamRobots は大きい配列を1件ずつ読む（§6.2）。
//
// A（全件同期）でやりたいのは「1件ずつ記憶と比べる」ことなので、
// 全件のスライスを作る必要がそもそもない。ここが 20倍のメモリ差になる。
func StreamRobots(r io.Reader, fn func(SlimRobot) error) error {
	d := json.NewDecoder(r)

	// { を読む
	tok, err := d.Token()
	if err != nil {
		return fmt.Errorf("read object start: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("expected object start, got %v", tok)
	}

	for d.More() {
		keyTok, err := d.Token()
		if err != nil {
			return fmt.Errorf("read key: %w", err)
		}
		key, _ := keyTok.(string)
		if key != "robots" {
			// 興味のないキーは値ごと読み飛ばす（json.RawMessage は割り当てるので
			// skipValue で捨てる）。
			if err := skipValue(d); err != nil {
				return err
			}
			continue
		}
		// [ を読む
		if tok, err = d.Token(); err != nil {
			return fmt.Errorf("read array start: %w", err)
		} else if delim, ok := tok.(json.Delim); !ok || delim != '[' {
			return fmt.Errorf("expected array start, got %v", tok)
		}
		for d.More() {
			var one SlimRobot
			if err := d.Decode(&one); err != nil {
				return fmt.Errorf("decode element: %w", err)
			}
			if err := fn(one); err != nil {
				return err
			}
		}
		// ] を読む
		if _, err := d.Token(); err != nil {
			return fmt.Errorf("read array end: %w", err)
		}
	}
	return nil
}

func skipValue(d *json.Decoder) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // スカラーは1トークンで終わり
	}
	if delim != '{' && delim != '[' {
		return fmt.Errorf("unexpected delim %v", delim)
	}
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// UnknownFieldRecorder は「知らない項目を起動時に1回だけ記録し、処理は止めない」
// ための仕組み（§6.3）。DisallowUnknownFields の代わりにこれを使う。
//
// DisallowUnknownFields は API に項目が1つ足されただけで全件同期を止めてしまう。
type UnknownFieldRecorder struct {
	once     sync.Once
	mu       sync.Mutex
	found    []string
	reported bool
}

// Inspect は1件ぶんの生 JSON を、既知フィールド集合と突き合わせる。
// 初回の1件しか見ず、結果を返すのも1回だけ（起動時に1回だけ記録する、§6.3）。
// 毎メッセージ返すと、ログが埋まって「気づける」どころではなくなる。
func (u *UnknownFieldRecorder) Inspect(raw []byte, known map[string]struct{}) []string {
	u.once.Do(func() {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return
		}
		var out []string
		for k := range probe {
			if _, ok := known[k]; !ok {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		u.mu.Lock()
		u.found = out
		u.mu.Unlock()
	})
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.reported {
		return nil
	}
	u.reported = true
	return u.found
}

// Found は記録済みの未知項目を、何度でも読み出せる形で返す（状態表示用）。
func (u *UnknownFieldRecorder) Found() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.found...)
}

// KnownFields は構造体タグから既知フィールド集合を作る補助。
func KnownFields(tags ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		m[strings.TrimSpace(t)] = struct{}{}
	}
	return m
}

// SlimKnownFields は SlimRobot が知っている項目。
func SlimKnownFields() map[string]struct{} {
	return KnownFields("id", "status", "online", "battery", "observed_at")
}
