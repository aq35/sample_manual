#!/usr/bin/env bash
# 資料の主張を、ぜんぶ実行して確かめる。結果は docs/results/ に残す。
#
#   ./scripts/run-all.sh
#
# MYSQL_DSN が未設定なら、DB を使う項目は skip される（Go 単体の項目は動く）。
set -uo pipefail
cd "$(dirname "$0")/.."

OUT=docs/results
mkdir -p "$OUT"
stamp=$(date +%Y%m%d-%H%M%S)

run() { # run <出力ファイル> <説明> <コマンド...>
  local file="$1" desc="$2"; shift 2
  echo "==> ${desc}"
  {
    echo "# ${desc}"
    echo "# 実行: $(date -Is)"
    echo "# コマンド: $*"
    echo
    "$@" 2>&1
  } | tee "${OUT}/${file}"
  echo
}

echo "Go: $(go version)"
if [[ -n "${MYSQL_DSN:-}" ]]; then
  echo "MYSQL_DSN: 設定あり"
else
  echo "MYSQL_DSN: 未設定（DB を使う項目は skip されます）"
fi
echo

run "01-tests-${stamp}.txt"      "① 設計チェックリストのテスト（§2〜§6）" \
    go test ./... -v -timeout 10m
run "02-json-bench-${stamp}.txt" "② JSON の扱い（§6.1 / §6.2）" \
    go test ./internal/jsonx/ -bench . -benchmem -run XXX -count=3
run "03-compare-bench-${stamp}.txt" "③ 状態比較のコスト（§8）" \
    go test ./internal/model/ -bench Compare -benchmem -run XXX -count=3
run "04-memory-${stamp}.txt"     "④ 1対象あたりのメモリ（§3.3）" \
    go test ./internal/worker/ -run Memory -v

if [[ -n "${MYSQL_DSN:-}" ]]; then
  run "05-mysql-facts-${stamp}.txt" "⑤ MySQL の挙動確認（§9 の未検証項目）" \
      go test ./internal/mysqlfacts/ -v -timeout 10m
  run "06-write-bench-${stamp}.txt" "⑥ 書き込み方法の比較（§4.3）" \
      go test ./internal/store/ -bench Write -benchtime 30x -run XXX -count=3
  run "07-loadsim-${stamp}.txt"     "⑦ 負荷シミュレーション（§4.1 の表）" \
      go run ./cmd/loadsim -rate 1000 -duration 10s -robots 1000 -change-rate 0.01
  run "08-repo-experiments-${stamp}.txt" "⑧ リポジトリ層の実験（表設計・読み方・事故）" \
      go test ./internal/repo/ -run TestExperiment_ -v -timeout 20m
  run "09-repo-guards-${stamp}.txt" "⑨ リポジトリ層が事故を止めることの確認" \
      go test ./internal/repo/... -run 'TestScope|TestRepo|TestTx|TestPage|TestCheck|TestBind|TestExpect|TestHarden' -v -timeout 10m
  run "10-repo-bench-${stamp}.txt" "⑩ リポジトリ層の代金" \
      go test ./internal/repo/ -bench 'PointRead|CheckAndBind' -benchmem -run XXX -count=3
  run "11-locking-${stamp}.txt" "⑪ 排他制御の実験（GET_LOCK の性質と代替）" \
      go test ./internal/repo/ -run 'TestExperimentLock_|TestWithLock|TestMigrate' -v -timeout 10m

  # ---- 異常系の実験（EXP-1..3, 6）。結果は docs/results/exp-N/ にも保存される ----
  run "12-exp1-effect-crash-${stamp}.txt" "⑫ EXP-1 外部 effect 途中の SIGKILL" \
      go test ./internal/effectlab/ -run TestEXP1 -v -timeout 20m
  run "13-exp2-fencing-${stamp}.txt" "⑬ EXP-2 lease / fencing / 時計のずれ" \
      go test ./internal/fencelab/ -run TestEXP2 -v -timeout 20m
  run "14-exp3-shutdown-${stamp}.txt" "⑭ EXP-3 graceful shutdown" \
      go test ./internal/shutdownlab/ -run TestEXP3 -v -timeout 20m
  run "15-exp6-migration-crash-${stamp}.txt" "⑮ EXP-6 マイグレーション途中の crash" \
      go test ./internal/repo/ -run TestEXP6 -v -timeout 20m
fi

echo "結果は ${OUT}/ に残した"
