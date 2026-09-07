-- 1テーブル・status 索引あり（実務の既定）。status で絞る一覧はこの索引で狙い撃つ
CREATE TABLE IF NOT EXISTS st_one (
  tenant_id  VARCHAR(32) NOT NULL,
  id         BIGINT      NOT NULL,
  status     TINYINT UNSIGNED NOT NULL,
  created_at DATETIME    NOT NULL,
  PRIMARY KEY (tenant_id, id),
  KEY idx_status (tenant_id, status, id)
) ENGINE=InnoDB;

-- 1テーブル・status 索引なし（比較用: 索引が無いと status 絞りが全表走査になる）
CREATE TABLE IF NOT EXISTS st_noidx (
  tenant_id  VARCHAR(32) NOT NULL,
  id         BIGINT      NOT NULL,
  status     TINYINT UNSIGNED NOT NULL,
  created_at DATETIME    NOT NULL,
  PRIMARY KEY (tenant_id, id)
) ENGINE=InnoDB;

-- 状態で表を分ける案（active / terminal）。状態遷移はこの2表を跨いだ「引っ越し」になる
CREATE TABLE IF NOT EXISTS st_active (
  tenant_id  VARCHAR(32) NOT NULL,
  id         BIGINT      NOT NULL,
  status     TINYINT UNSIGNED NOT NULL,
  created_at DATETIME    NOT NULL,
  PRIMARY KEY (tenant_id, id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS st_terminal (
  tenant_id  VARCHAR(32) NOT NULL,
  id         BIGINT      NOT NULL,
  status     TINYINT UNSIGNED NOT NULL,
  created_at DATETIME    NOT NULL,
  PRIMARY KEY (tenant_id, id)
) ENGINE=InnoDB;

-- hot/cold の hot（終端を隔離した後の active 専用表・索引あり）
CREATE TABLE IF NOT EXISTS st_hot (
  tenant_id  VARCHAR(32) NOT NULL,
  id         BIGINT      NOT NULL,
  status     TINYINT UNSIGNED NOT NULL,
  created_at DATETIME    NOT NULL,
  PRIMARY KEY (tenant_id, id),
  KEY idx_status (tenant_id, status, id)
) ENGINE=InnoDB;
