package jsonlab_test

// EXP-69: worker 受信ペイロードのパースを encoding/json(v1) / map[string]any / encoding/json/v2 で比べる。
//
//	go test ./internal/jsonlab/ -run TestEXP69 -v                 # v1 と map（v2 は skip）
//	GOEXPERIMENT=jsonv2 go test ./internal/jsonlab/ -run TestEXP69 -v  # v2 も含む
//
// §6.1 の「map[string]any は構造体展開より重い」を再確認しつつ、v2 が v1 とどう変わるかを
// 同じ入力で ns/op・B/op・allocs/op で測る。v2 は GOEXPERIMENT=jsonv2 のときだけ。

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/jsonlab"
)

func TestEXP69_jsonパースコスト(t *testing.T) {
	ctx := context.Background()
	const n = 1000
	payload := jsonlab.Sample(n)

	rec := expkit.NewRecorder("EXP-69", "json-parse-cost",
		"受信ペイロードのパースを v1 struct / map[string]any / json/v2 で比べる")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(strings.Join([]string{
		"map[string]any は構造体展開より重い（§6.1）。encoding/json/v2 は同じ構造体展開で v1 と",
		"同等〜改善のはず（少なくとも桁で悪化しない）。同じ入力で ns/op・B/op・allocs/op を比べる。",
		"v2 は GOEXPERIMENT=jsonv2 のときだけ測る（未有効なら skip し、その事実を記録する）。",
	}, " "))
	rec.Workload("items", n).Workload("payload_bytes", len(payload))
	rec.Injection("jsonv2_available", jsonlab.JSONV2Available)

	bench := func(fn func([]byte) (int, error)) testing.BenchmarkResult {
		return testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := fn(payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	add := func(name string, accident bool, r testing.BenchmarkResult) {
		rec.Add(expkit.Variant{
			Name:     name,
			Accident: accident,
			Metrics: map[string]float64{
				"ns_per_op":     float64(r.NsPerOp()),
				"bytes_per_op":  float64(r.AllocedBytesPerOp()),
				"allocs_per_op": float64(r.AllocsPerOp()),
			},
		})
	}

	structV1 := bench(jsonlab.ParseStructV1)
	mapV1 := bench(jsonlab.ParseMapV1)
	add("v1 構造体展開（[]Item）", false, structV1)
	add("v1 map[string]any（§6.1 の重い受け方）", true, mapV1)
	t.Logf("v1 struct: %d ns/op %d B/op %d allocs/op", structV1.NsPerOp(), structV1.AllocedBytesPerOp(), structV1.AllocsPerOp())
	t.Logf("v1 map   : %d ns/op %d B/op %d allocs/op", mapV1.NsPerOp(), mapV1.AllocedBytesPerOp(), mapV1.AllocsPerOp())

	var v2 testing.BenchmarkResult
	if jsonlab.JSONV2Available {
		v2 = bench(jsonlab.ParseStructV2)
		add("v2 構造体展開（encoding/json/v2）", false, v2)
		t.Logf("v2 struct: %d ns/op %d B/op %d allocs/op", v2.NsPerOp(), v2.AllocedBytesPerOp(), v2.AllocsPerOp())
	} else {
		rec.Add(expkit.Variant{
			Name:  "v2 構造体展開: skip（GOEXPERIMENT=jsonv2 未有効）",
			Notes: []string{"GOEXPERIMENT=jsonv2 go test で測れる"},
		})
		t.Log("v2 は GOEXPERIMENT=jsonv2 未有効のため skip")
	}

	// ---- 検証 ----
	// map は構造体展開より割り当てが多い（§6.1 の再確認）
	if mapV1.AllocsPerOp() <= structV1.AllocsPerOp() {
		t.Errorf("map の割り当てが構造体以下（§6.1 と矛盾）: map=%d struct=%d",
			mapV1.AllocsPerOp(), structV1.AllocsPerOp())
	}
	// v2 は v1 struct に対して桁で悪化しない（あれば）
	if jsonlab.JSONV2Available && v2.NsPerOp() > structV1.NsPerOp()*3 {
		t.Errorf("v2 が v1 struct より3倍以上遅い（想定外）: v2=%d v1=%d", v2.NsPerOp(), structV1.NsPerOp())
	}

	ratioAlloc := float64(mapV1.AllocsPerOp()) / float64(max64(structV1.AllocsPerOp(), 1))
	rec.Scope(
		"同一入力（"+strconv.Itoa(n)+"件の JSON 配列・"+strconv.Itoa(len(payload))+"B）を Unmarshal",
		"struct=[]Item / map=[]map[string]any / v2=encoding/json/v2 の struct",
		"Go "+runtime.Version()+" / testing.Benchmark（自動反復）",
	)
	rec.Uncertain(
		"絶対値は環境依存。桁の関係が要点（§6.1 と同じ約束）",
		"v2 は実験機能。将来 API・性能が変わりうる（GOEXPERIMENT=jsonv2 前提）",
		"実運用は「大きい配列は1件ずつ stream で読む（§6.2）」も併用する。ここは一括 Unmarshal の比較",
	)
	rec.Artifact(
		"internal/jsonlab: Sample / ParseStructV1 / ParseMapV1 / ParseStructV2（build tag 分離）",
		"docs/json-v2.md: v1/map/v2 のパースコスト比較",
	)
	rec.Next("なし")

	verdict := []string{
		"map[string]any は構造体展開より割り当てが多い（§6.1 を再確認・約" +
			strconv.FormatFloat(ratioAlloc, 'f', 1, 64) + "倍）。受信は必ず構造体で受ける。",
	}
	if jsonlab.JSONV2Available {
		verdict = append(verdict, "encoding/json/v2 は v1 struct と同等の桁（詳細は receipt の数値）。")
	} else {
		verdict = append(verdict, "v2 は GOEXPERIMENT=jsonv2 でのみ測定（この実行では skip）。")
	}
	files, err := rec.Save(strings.Join(verdict, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("結果: %v", files)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
