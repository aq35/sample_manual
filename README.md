# ロボット動作スケジューリング 設計書

---

## 1. 概要

ロボットの動作をポイント単位で管理し、定期配送・臨時配送・メンテナンスをスケジュールとタスクで制御するシステムのDB設計。

---

## 2. 概念整理

| 概念 | 説明 |
|---|---|
| ポイント | ロボットが移動する地点 |
| スケジュール | いつ・どの条件で動かすかの定義 |
| ルールセット | スケジュールの繰り返し条件（週次/月次/固定/範囲） |
| タスク | 実際に実行される配送インスタンス |
| 帰還先 | タスク完了後にロボットが戻るポイント |

---

## 3. 優先順位

```
1. メンテナンス（maintenance_mode フラグ）   → 即時停止
2. メンテナンス（スケジュール期間中）        → タスク実行しない
3. pending タスクあり                       → created_at 昇順で実行（FIFO）
4. pending タスクなし                       → 帰還ポイントで待機
```

---

## 4. テーブル一覧

| テーブル名 | 説明 |
|---|---|
| `points` | ポイントマスタ |
| `robots` | ロボットマスタ |
| `schedule_rule_sets` | ルールセット |
| `schedule_rules` | ルール明細（週次/月次/固定/範囲） |
| `robot_schedules` | スケジュール（operation / maintenance / return_point 統合） |
| `schedule_points` | 巡回ポイント順序（operation用） |
| `delivery_tasks` | 実行タスク（定期・臨時共通） |
| `delivery_task_points` | タスクのポイント順序・実績 |

---

## 5. テーブル定義

### 5.1 `points`（ポイントマスタ）

| カラム | 型 | 説明 |
|---|---|---|
| `id` | UUID PK | |
| `name` | VARCHAR | ポイント名 |
| `x` | FLOAT | X座標 |
| `y` | FLOAT | Y座標 |
| `description` | TEXT nullable | 備考 |
| `created_at` | TIMESTAMP | |
| `updated_at` | TIMESTAMP | |

---

### 5.2 `robots`（ロボットマスタ）

| カラム | 型 | 説明 |
|---|---|---|
| `id` | UUID PK | |
| `name` | VARCHAR | ロボット名 |
| `maintenance_mode` | BOOLEAN | 即時停止フラグ |
| `status` | ENUM | `active` / `maintenance` / `error` |
| `created_at` | TIMESTAMP | |
| `updated_at` | TIMESTAMP | |

---

### 5.3 `schedule_rule_sets`（ルールセット）

| カラム | 型 | 説明 |
|---|---|---|
| `id` | UUID PK | |
| `name` | VARCHAR | 例：「平日」「毎月1日・15日」 |
| `created_at` | TIMESTAMP | |

**利点：** 同じルールを複数スケジュールで共有・再利用できる。変更が1箇所で済む。

---

### 5.4 `schedule_rules`（ルール明細）

| カラム | 型 | 説明 |
|---|---|---|
| `id` | UUID PK | |
| `rule_set_id` | UUID FK | |
| `rule_type` | ENUM | `weekly` / `monthly` / `fixed` / `range` |
| `day_of_week` | INT nullable | weekly用・ビットフラグ（1〜127） |
| `day_of_month` | INT nullable | monthly用・1〜31（複数行で複数日） |
| `fixed_date` | DATE nullable | fixed用 |
| `range_start` | DATE nullable | range用・開始日 |
| `range_end` | DATE nullable | range用・終了日 |
| `created_at` | TIMESTAMP | |

#### rule_type 別の使用カラム

| rule_type | 使うカラム | 例 |
|---|---|---|
| `weekly` | `day_of_week` | 62 = 平日 |
| `monthly` | `day_of_month` | 1行1日（複数行で複数日） |
| `fixed` | `fixed_date` | 2024-12-31 |
| `range` | `range_start` / `range_end` | 2024-04-01〜2024-06-30 |

#### day_of_week ビットフラグ

| 曜日 | ビット | 値 |
|---|---|---|
| 日 | 0 | 1 |
| 月 | 1 | 2 |
| 火 | 2 | 4 |
| 水 | 3 | 8 |
| 木 | 4 | 16 |
| 金 | 5 | 32 |
| 土 | 6 | 64 |

| パターン | 値 |
|---|---|
| 平日（月〜金） | 62 |
| 月・水・金 | 42 |
| 毎日 | 127 |
| 土曜 | 64 |

---

### 5.5 `robot_schedules`（スケジュール統合）

operation / maintenance / return_point を1テーブルに統合。

| カラム | 型 | 説明 |
|---|---|---|
| `id` | UUID PK | |
| `robot_id` | UUID FK | |
| `rule_set_id` | UUID FK | |
| `name` | VARCHAR nullable | スケジュール名 |
| `category` | ENUM | `operation` / `maintenance` / `return_point` |
| `start_time` | TIME | 動作開始時刻 |
| `end_time` | TIME | 動作終了時刻 |
| `point_id` | UUID FK nullable | 帰還先ポイント（return_point用） |
| `priority` | INT nullable | 帰還先優先順位（return_point用） |
| `active` | BOOLEAN | 有効/無効 |
| `created_at` | TIMESTAMP | |
| `updated_at` | TIMESTAMP | |

#### category 別の使用カラム

| category | name | point_id | priority |
|---|---|---|---|
| `operation` | ✅ | ❌ | ❌ |
| `maintenance` | ✅ | ❌ | ❌ |
| `return_point` | ❌ | ✅ | ✅ |

#### 帰還先の判定ロジック

```
タスク完了時
  └─ return_point スケジュールから現在日時にマッチするものを取得
        ├─ マッチあり → priority 高いものの point_id へ帰還
        └─ マッチなし → デフォルト帰還ポイントへ
```

---

### 5.6 `schedule_points`（巡回ポイント順序）

operation スケジュールのポイントテンプレート。

| カラム | 型 | 説明 |
|---|---|---|
| `id` | UUID PK | |
| `schedule_id` | UUID FK | |
| `point_id` | UUID FK | |
| `order` | INT | 実行順 |
| `dwell_seconds` | INT | 滞在秒数 |

---

### 5.7 `delivery_tasks`（実行タスク）

定期配送・臨時配送を統合。FIFO（created_at 昇順）で処理。

| カラム | 型 | 説明 |
|---|---|---|
| `id` | UUID PK | |
| `robot_id` | UUID FK | |
| `task_type` | ENUM | `scheduled`（定期）/ `ad_hoc`（臨時） |
| `source_schedule_id` | UUID FK nullable | 定期配送の場合・元スケジュールID |
| `status` | ENUM | `pending` / `running` / `done` / `cancelled` |
| `triggered_by` | UUID nullable | 臨時配送の操作者ID |
| `scheduled_start_at` | TIMESTAMP nullable | 予定開始日時 |
| `started_at` | TIMESTAMP nullable | 実際の開始日時 |
| `ended_at` | TIMESTAMP nullable | 実際の終了日時 |
| `created_at` | TIMESTAMP | |

#### task_type の違い

| task_type | source_schedule_id | triggered_by | 生成タイミング |
|---|---|---|---|
| `scheduled` | 必須 | NULL | スケジュールから自動生成 |
| `ad_hoc` | NULL | 必須 | 手動トリガーで即時生成 |

---

### 5.8 `delivery_task_points`（タスクのポイント実績）

| カラム | 型 | 説明 |
|---|---|---|
| `id` | UUID PK | |
| `task_id` | UUID FK | |
| `point_id` | UUID FK | |
| `order` | INT | 実行順 |
| `arrived_at` | TIMESTAMP nullable | 実績到着時刻（NULL=未到着） |

---

## 6. 全体関係図

```
schedule_rule_sets
  └─ schedule_rules (weekly/monthly/fixed/range)
        ↑ 共有・再利用
robot_schedules (operation / maintenance / return_point)
  ├─ rule_set_id → schedule_rule_sets
  ├─ schedule_points → points        (operation用・巡回ポイント)
  └─ point_id → points               (return_point用・帰還先)

delivery_tasks (scheduled / ad_hoc)
  ├─ source_schedule_id → robot_schedules
  └─ delivery_task_points → points

robots
  ├─ robot_schedules
  └─ delivery_tasks
```

---

## 7. レコード例

### schedule_rules レコード例

| rule_set_id | rule_type | day_of_week | day_of_month | fixed_date | range_start | range_end |
|---|---|---|---|---|---|---|
| rs-001 | weekly | 62 | NULL | NULL | NULL | NULL |
| rs-002 | monthly | NULL | 1 | NULL | NULL | NULL |
| rs-002 | monthly | NULL | 15 | NULL | NULL | NULL |
| rs-003 | fixed | NULL | NULL | 2024-12-31 | NULL | NULL |
| rs-004 | range | NULL | NULL | NULL | 2024-04-01 | 2024-06-30 |

### delivery_tasks レコード例

| task_type | source_schedule_id | status | triggered_by | created_at |
|---|---|---|---|---|
| scheduled | sc-001 | done | NULL | 2024-01-08 08:00 |
| scheduled | sc-001 | running | NULL | 2024-01-10 08:00 |
| ad_hoc | NULL | pending | user-001 | 2024-01-10 10:30 |
