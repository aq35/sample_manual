// Package jsonlab は EXP-69（encoding/json/v2 のパースコスト）の実験本体。
//
// §6.1 で「map[string]any は重い（構造体展開より 15〜26 倍のメモリ・22〜30 倍の割り当て）」を実測した。
// Go 1.25/1.26 で実験的に入った encoding/json/v2（GOEXPERIMENT=jsonv2）で、worker の受信ペイロード
// パースが v1 とどう変わるかを、同じ入力で ns/op・B/op・allocs/op で比べる。
//
// v2 は GOEXPERIMENT=jsonv2 でビルドしたときだけ有効。ビルドタグで分離し、
// 未有効のときは go build/test が普通に通る（v2 は skip）。
package jsonlab

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Item は worker が受け取る同期ペイロードの1件（構造体で受ける版）。
type Item struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Battery float64  `json:"battery"`
	Updated string   `json:"updated_at"`
	Tags    []string `json:"tags"`
}

// Sample は n 件の JSON 配列を作る（受信ペイロードを模す）。
func Sample(n int) []byte {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{
			ID:      int64(i),
			Name:    fmt.Sprintf("robot-%05d", i),
			Status:  []string{"pending", "in_progress", "completed"}[i%3],
			Battery: float64(i%100) + 0.5,
			Updated: "2026-09-10T00:00:00Z",
			Tags:    []string{"a", "b", strings.Repeat("t", i%8)},
		}
	}
	b, err := json.Marshal(items)
	if err != nil {
		panic(err)
	}
	return b
}

// ParseStructV1 は encoding/json(v1) で []Item に展開する。
func ParseStructV1(b []byte) (int, error) {
	var xs []Item
	if err := json.Unmarshal(b, &xs); err != nil {
		return 0, err
	}
	return len(xs), nil
}

// ParseMapV1 は map[string]any で受ける（§6.1 の重い受け方・比較用）。
func ParseMapV1(b []byte) (int, error) {
	var xs []map[string]any
	if err := json.Unmarshal(b, &xs); err != nil {
		return 0, err
	}
	return len(xs), nil
}
