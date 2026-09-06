-- 幅広い行（現在状態と大きなペイロードを同じ行に混ぜる = §4.4 に反する設計）
CREATE TABLE IF NOT EXISTS wide_state (
  tenant_id  VARCHAR(32)      NOT NULL,
  robot_id   VARCHAR(32)      NOT NULL,
  status     TINYINT UNSIGNED NOT NULL,
  battery    TINYINT          NOT NULL,
  payload    VARCHAR(2000)    NOT NULL DEFAULT '',  -- 大きな列を同居させる
  updated_at DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, robot_id),
  KEY k_status (tenant_id, status)                  -- 更新のたびに保守される二次索引
) ENGINE=InnoDB;

-- 狭い行（熱い現在状態だけ）。大きな列は別表へ（§4.4 の分割）
CREATE TABLE IF NOT EXISTS narrow_state (
  tenant_id  VARCHAR(32)      NOT NULL,
  robot_id   VARCHAR(32)      NOT NULL,
  status     TINYINT UNSIGNED NOT NULL,
  battery    TINYINT          NOT NULL,
  updated_at DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, robot_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS narrow_payload (
  tenant_id VARCHAR(32)   NOT NULL,
  robot_id  VARCHAR(32)   NOT NULL,
  payload   VARCHAR(2000) NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, robot_id)
) ENGINE=InnoDB;

-- 追記履歴・パーティションあり（日付 RANGE。古いのは DROP PARTITION で消す）
CREATE TABLE IF NOT EXISTS hist_part (
  tenant_id     VARCHAR(32) NOT NULL,
  robot_id      VARCHAR(32) NOT NULL,
  observed_date DATE        NOT NULL,
  observed_at   DATETIME(3) NOT NULL,
  status        TINYINT UNSIGNED NOT NULL,
  PRIMARY KEY (tenant_id, robot_id, observed_date, observed_at)
) ENGINE=InnoDB
PARTITION BY RANGE COLUMNS(observed_date) (
  PARTITION p1 VALUES LESS THAN ('2026-01-02'),
  PARTITION p2 VALUES LESS THAN ('2026-01-03'),
  PARTITION p3 VALUES LESS THAN ('2026-01-04'),
  PARTITION pmax VALUES LESS THAN (MAXVALUE)
);

-- 追記履歴・パーティションなし（古いのは DELETE で消す）
CREATE TABLE IF NOT EXISTS hist_plain (
  tenant_id     VARCHAR(32) NOT NULL,
  robot_id      VARCHAR(32) NOT NULL,
  observed_date DATE        NOT NULL,
  observed_at   DATETIME(3) NOT NULL,
  status        TINYINT UNSIGNED NOT NULL,
  PRIMARY KEY (tenant_id, robot_id, observed_date, observed_at),
  KEY k_date (observed_date)
) ENGINE=InnoDB;
