#!/usr/bin/env bash
# 資料の主張を、ぜんぶ実行して確かめる。
#
#   ./scripts/run-all.sh              # 実行ログは .run-logs/ へ（git 管理外）。receipt は書かない
#   EXP_RECORD=1 ./scripts/run-all.sh # 実験の receipt を docs/results/ に固定名で保存する
#
# ★通常実行は repo を汚さない。生ログは .run-logs/（.gitignore 済）へ出す。
# 実験の receipt（docs/results/<unit>/）を更新するのは EXP_RECORD=1 のときだけ。
# これが無いと full suite のたびに result が書き換わり、作業ツリーが dirty になる
# （実際にそれで doc のリンクが毎回ずれた。scripts/postflight.sh の worktree gate が捕まえる）。
#
# MYSQL_DSN が未設定なら、DB を使う項目は skip される（Go 単体の項目は動く）。
set -uo pipefail
cd "$(dirname "$0")/.."

OUT=${RUN_LOG_DIR:-.run-logs}
mkdir -p "$OUT"
stamp=$(date +%Y%m%d-%H%M%S)

# 実験の前に MySQL の baseline を取る（postflight で使う）。
BASELINE=""
if [[ -n "${MYSQL_DSN:-}" ]]; then
  BASELINE=$(scripts/postflight.sh baseline 2>/dev/null || echo "")
fi

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
  run "16-exp4-backpressure-${stamp}.txt" "⑯ EXP-4 backpressure と過負荷" \
      go test ./internal/loadlab/ -run TestEXP4 -v -timeout 20m
  run "17-exp5-pool-${stamp}.txt" "⑰ EXP-5 接続プールの飽和点" \
      go test ./internal/poollab/ -run TestEXP5 -v -timeout 20m
  run "18-exp7-plan-${stamp}.txt" "⑱ EXP-7 実行計画とデータの偏り" \
      go test ./internal/planlab/ -run TestEXP7 -v -timeout 20m
  run "22-exp11-backup-${stamp}.txt" "㉒ EXP-11 バックアップ・復元・破損" \
      go test ./internal/backuplab/ -run TestEXP11 -v -timeout 20m
fi

# ---- postflight: 「テストが exit 0」だけを成功条件にしない ----
# tests exit 0 AND MySQL alive AND read/write probe AND 接続が baseline へ戻る
# AND プロセス残存なし AND 想定外スキーマなし AND 作業ツリー clean AND receipt manifest 妥当
POSTFLIGHT_OK=1
if [[ -n "${MYSQL_DSN:-}" && -n "$BASELINE" ]]; then
  echo "==> postflight（健全性ゲート）"
  if ! scripts/postflight.sh check "run-all" $BASELINE; then POSTFLIGHT_OK=0; fi
  # 想定外のデータベースが増えていないか（実験が後始末を忘れていないか）
  extra=$(mysql -uroot -N -e "SHOW DATABASES" 2>/dev/null \
    | grep -vE '^(information_schema|mysql|performance_schema|sys|workerdb|workerdb2|postflight_probe)$' || true)
  if [[ -n "$extra" ]]; then echo "POSTFLIGHT FAIL: 想定外のデータベース: $extra"; POSTFLIGHT_OK=0; fi
fi
# tracked に実行形式が紛れていないか
if ! scripts/check-no-binaries.sh >/dev/null 2>&1; then
  echo "POSTFLIGHT FAIL: tracked に実行形式が含まれている（scripts/check-no-binaries.sh）"; POSTFLIGHT_OK=0
fi
# 作業ツリーが clean か（EXP_RECORD 実行では receipt 更新を除いて判定）
dirty=$(git status --porcelain | grep -vE '^\?\? \.run-logs/' || true)
if [[ -z "${EXP_RECORD:-}" && -n "$dirty" ]]; then
  echo "POSTFLIGHT FAIL: 作業ツリーが dirty（通常実行は repo を書き換えないはず）:"; echo "$dirty" | head
  POSTFLIGHT_OK=0
fi
# receipt manifest（latest.json）が妥当か
for lj in docs/results/exp-*/latest.json; do
  [ -f "$lj" ] || continue
  if ! grep -q '"meter_version"' "$lj"; then echo "POSTFLIGHT FAIL: $lj に meter_version が無い"; POSTFLIGHT_OK=0; fi
done

if [[ "$POSTFLIGHT_OK" -eq 1 ]]; then
  echo "POSTFLIGHT OK: MySQL 生存・probe・接続 baseline・プロセス/スキーマ/tree・manifest すべて通過"
else
  echo "POSTFLIGHT FAILED: 上のいずれかが崩れている。exit 0 だけを見て VERIFIED とみなさないこと"
fi

# ---- MySQL が無くても走る実験 ----
run "19-exp8-guard-fuzz-${stamp}.txt" "⑲ EXP-8 SQL 検査の fuzz（回帰入力ぶんのみ）" \
    go test ./internal/repo/ -run 'TestEXP8|TestGuardProperties' -v -timeout 10m
run "20-exp9-static-analysis-${stamp}.txt" "⑳ EXP-9 保守性の自動検査" \
    go test ./internal/lint/ -run 'TestEXP9|TestAnalyzers|TestEscapeHatch' -v -timeout 10m
# EXP-10 は MYSQL_DSN があれば両エンジンを突き合わせ、無ければ SQLite 側だけを測って
# MySQL 側は UNVERIFIED として残す（測れなかったことを結論にしない）
run "21-exp10-sqlite-${stamp}.txt" "㉑ EXP-10 SQLite companion" \
    go test ./internal/sqlitefacts/ -run TestEXP10 -v -timeout 20m

echo "生ログは ${OUT}/ に残した（git 管理外）。receipt は EXP_RECORD=1 のときだけ docs/results/ に保存"
exit $(( POSTFLIGHT_OK == 1 ? 0 : 1 ))
