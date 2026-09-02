package jsonx_test

// §6.1 / §6.2 の表を、そのまま実行して確かめるためのベンチマーク。
//
//	go test ./internal/jsonx/ -bench . -benchmem -count=3
//
// 見るのは絶対値ではなく B/op と allocs/op の比。マシンが変わっても比は変わらない。

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/aq35/sample_manual/internal/fixture"
	"github.com/aq35/sample_manual/internal/jsonx"
)

var payload = fixture.RobotsJSON(1000)

// ---- §6.1 1,000件を1回解析する（3つのやり方） ----

// 要る項目だけの構造体。これが推奨。
func BenchmarkParse_SlimStruct(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		var resp jsonx.SlimResponse
		if err := json.Unmarshal(payload, &resp); err != nil {
			b.Fatal(err)
		}
		if len(resp.Robots) != 1000 {
			b.Fatalf("got %d", len(resp.Robots))
		}
	}
}

// 全項目を書いた構造体。「将来使うかもしれないから」の代金がここに出る。
func BenchmarkParse_FatStruct(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		var resp jsonx.FatResponse
		if err := json.Unmarshal(payload, &resp); err != nil {
			b.Fatal(err)
		}
		if len(resp.Robots) != 1000 {
			b.Fatalf("got %d", len(resp.Robots))
		}
	}
}

// map[string]any。使わないこと（§6.1）。
func BenchmarkParse_MapAny(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil {
			b.Fatal(err)
		}
		if len(m["robots"].([]any)) != 1000 {
			b.Fatal("unexpected shape")
		}
	}
}

// ---- §6.2 受け取り方3つ（body を読むところから） ----

// bytes.Reader を HTTP レスポンスボディ相当として使う。
// httptest を挟むとサーバ側の割り当てが同じプロセスに乗り、B/op が濁るため。
func body() io.Reader { return bytes.NewReader(payload) }

// ① 全部読んでから Unmarshal。本文まるごと＋全件のスライスを同時に持つ。
func BenchmarkReceive_ReadAllThenUnmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		raw, err := io.ReadAll(body())
		if err != nil {
			b.Fatal(err)
		}
		var resp jsonx.SlimResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			b.Fatal(err)
		}
		sink = len(resp.Robots)
	}
}

// ② Decoder で全体を一度に。読み込みバッファは減るが、全件のスライスは残る。
func BenchmarkReceive_DecodeWhole(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		var resp jsonx.SlimResponse
		if err := json.NewDecoder(body()).Decode(&resp); err != nil {
			b.Fatal(err)
		}
		sink = len(resp.Robots)
	}
}

// ③ Decoder で配列を1件ずつ。全件のスライスを作らない（§6.2）。
func BenchmarkReceive_StreamEach(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		n := 0
		err := jsonx.StreamRobots(body(), func(r jsonx.SlimRobot) error {
			n++ // 実際にはここで記憶と比べて捨てる
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
		sink = n
	}
}

// ---- WebSocket 1件（§6.2 の使い分け） ----

var one = fixture.OneRobotJSON()

func BenchmarkReceive_SingleUnmarshal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		var r jsonx.SlimRobot
		if err := json.Unmarshal(one, &r); err != nil {
			b.Fatal(err)
		}
		sink = len(r.ID)
	}
}

var sink int
