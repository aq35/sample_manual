-- §4.4「表を分ける」/ §4.6「MySQL（InnoDB）固有」をそのまま形にしたスキーマ。
-- MySQL 8.0 / InnoDB 前提。

-- ① 現在状態: 1対象1行を UPDATE する。行数は対象数で固定（追記型にしない）。
CREATE TABLE IF NOT EXISTS robot_state (
  tenant_id   VARCHAR(32)      NOT NULL,
  robot_id    VARCHAR(32)      NOT NULL,
  status      TINYINT UNSIGNED NOT NULL,  -- model.Status（string ではなく整数、§6.3）
  online      TINYINT(1)       NOT NULL,
  battery     TINYINT          NOT NULL,
  observed_at DATETIME(3)      NOT NULL,  -- いつ観測したか（§2.4）
  source      TINYINT UNSIGNED NOT NULL,  -- api / ws（§2.4）
  -- ★主キーは (tenant_id, robot_id)（§4.6）。
  -- InnoDB は主キー順に物理配置されるので、同一テナントの行が隣接し、
  -- テナント単位のバッチが最小ページ数で済む。UUID を主キーにしない。
  PRIMARY KEY (tenant_id, robot_id)
) ENGINE=InnoDB;

-- ② 履歴: 追記のみ・変化点だけ。保持期間を先に決め、パーティションで消す（§4.6）。
-- DELETE は undo 肥大・purge 遅延で非常に高くつくので使わない。
-- 注意: パーティションキーは全ユニークキーに含まれている必要があるため、
-- observed_date を主キーに含めている（MySQL は生成列をパーティションキーにできない）。
CREATE TABLE IF NOT EXISTS robot_state_history (
  tenant_id     VARCHAR(32)      NOT NULL,
  robot_id      VARCHAR(32)      NOT NULL,
  observed_date DATE             NOT NULL,
  observed_at   DATETIME(3)      NOT NULL,
  status        TINYINT UNSIGNED NOT NULL,
  online        TINYINT(1)       NOT NULL,
  battery       TINYINT          NOT NULL,
  source        TINYINT UNSIGNED NOT NULL,
  PRIMARY KEY (tenant_id, robot_id, observed_date, observed_at)
) ENGINE=InnoDB
PARTITION BY RANGE COLUMNS(observed_date) (
  PARTITION p20260901 VALUES LESS THAN ('2026-09-02'),
  PARTITION p20260902 VALUES LESS THAN ('2026-09-03'),
  PARTITION p20260903 VALUES LESS THAN ('2026-09-04'),
  PARTITION pmax      VALUES LESS THAN (MAXVALUE)
);

-- ③ めったに変わらない情報は、状態と同じ行に置かない（§4.4）。
CREATE TABLE IF NOT EXISTS robot_profile (
  tenant_id VARCHAR(32) NOT NULL,
  robot_id  VARCHAR(32) NOT NULL,
  name      VARCHAR(64) NOT NULL,
  model     VARCHAR(64) NOT NULL,
  serial    VARCHAR(64) NOT NULL,
  PRIMARY KEY (tenant_id, robot_id)
) ENGINE=InnoDB;

-- ④ リース: 1テナント1担当を保証する（§2.8）。これが無いと同じワーカーが
-- 複数コンテナで同時に動き、接続も書き込みも二重になる（普段は動いてしまう）。
CREATE TABLE IF NOT EXISTS worker_lease (
  tenant_id  VARCHAR(32)      NOT NULL,
  owner      VARCHAR(64)      NOT NULL,
  expires_at DATETIME(3)      NOT NULL,
  fence      BIGINT UNSIGNED  NOT NULL DEFAULT 0,  -- 担当が変わるたびに増える
  PRIMARY KEY (tenant_id)
) ENGINE=InnoDB;
