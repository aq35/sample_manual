-- 同居: 一覧の対象行に、めったに要らない大きなメモ（detail）を同じ行に置く
CREATE TABLE IF NOT EXISTS split_inline (
  tenant_id VARCHAR(32)  NOT NULL,
  id        BIGINT       NOT NULL,
  status    TINYINT UNSIGNED NOT NULL,
  detail    VARCHAR(2000) NOT NULL DEFAULT '',   -- 失敗理由メモ。一覧では要らない
  PRIMARY KEY (tenant_id, id)
) ENGINE=InnoDB;

-- 分割: 一覧の対象行は狭く保ち、メモは別表へ
CREATE TABLE IF NOT EXISTS split_list (
  tenant_id VARCHAR(32) NOT NULL,
  id        BIGINT      NOT NULL,
  status    TINYINT UNSIGNED NOT NULL,
  PRIMARY KEY (tenant_id, id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS split_detail (
  tenant_id VARCHAR(32)  NOT NULL,
  id        BIGINT       NOT NULL,
  detail    VARCHAR(2000) NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, id)
) ENGINE=InnoDB;
