-- スケジュール: 命令を生む「予定」。
-- once（一度きり）か interval（周期）。next_run_at が来たら command を1件生む。
CREATE TABLE IF NOT EXISTS cmd_schedule (
  tenant_id    VARCHAR(32)  NOT NULL,
  schedule_id  VARCHAR(32)  NOT NULL,
  robot_id     VARCHAR(32)  NOT NULL,
  command_type VARCHAR(32)  NOT NULL,
  payload      VARCHAR(255) NOT NULL DEFAULT '',
  kind         ENUM('once','interval') NOT NULL DEFAULT 'once',
  interval_ms  BIGINT       NULL,
  next_run_at  DATETIME(3)  NOT NULL,
  enabled      TINYINT(1)   NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, schedule_id),
  -- 「そろそろ動かす予定」を引く索引（テナント内・有効・時刻順）
  KEY due (tenant_id, enabled, next_run_at)
) ENGINE=InnoDB;

-- 命令: ロボットへの1つの指示。ワーカーがポーリングして捌くキュー（outbox 兼用）。
CREATE TABLE IF NOT EXISTS cmd_command (
  tenant_id     VARCHAR(32)  NOT NULL,
  command_id    VARCHAR(40)  NOT NULL,
  robot_id      VARCHAR(32)  NOT NULL,
  type          VARCHAR(32)  NOT NULL,
  payload       VARCHAR(255) NOT NULL DEFAULT '',
  -- 状態機械。unknown は EXP-1 の OUTCOME_UNKNOWN（出したが結果不明）。
  state         ENUM('pending','dispatched','acked','done','failed','unknown') NOT NULL DEFAULT 'pending',
  idem_key      VARCHAR(64)  NOT NULL,             -- 二重発行を防ぐ冪等キー
  fence         BIGINT       NOT NULL DEFAULT 0,   -- lease の fence。古い担当の発行を弾く
  scheduled_for DATETIME(3)  NOT NULL,             -- いつ実行すべきか（due 判定）
  dispatched_at DATETIME(3)  NULL,
  attempts      INT          NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, command_id),
  -- ★ワーカーのホットパス: 「テナント内・pending・due 到来」を時刻順に引く
  KEY poll (tenant_id, state, scheduled_for),
  UNIQUE KEY uq_idem (tenant_id, idem_key)
) ENGINE=InnoDB;

-- 実績: 命令の結果を「独立に観測」して残す（EXP-1: action の戻り値を receipt にしない）。
-- 1命令に対して複数回観測しうる（出した直後 unknown → 後で done、など）ので追記型。
CREATE TABLE IF NOT EXISTS cmd_result (
  tenant_id   VARCHAR(32)  NOT NULL,
  command_id  VARCHAR(40)  NOT NULL,
  observed_at DATETIME(3)  NOT NULL,
  status      ENUM('success','failure','timeout','unknown') NOT NULL,
  fence       BIGINT       NOT NULL DEFAULT 0,
  detail      VARCHAR(255) NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, command_id, observed_at)
) ENGINE=InnoDB;
