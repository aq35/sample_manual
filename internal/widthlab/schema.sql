-- 太い memo を VARCHAR で持つ（3KB。行に収まる大きさなので行の中＝inline）
CREATE TABLE IF NOT EXISTS w_varchar (
  tenant_id VARCHAR(32) NOT NULL,
  id        BIGINT      NOT NULL,
  status    TINYINT UNSIGNED NOT NULL,
  memo      VARCHAR(2000) NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, id)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC;

-- 同じ 3KB を TEXT で持つ。TEXT でも行に収まる大きさなら inline に置かれる（＝行が太る）
CREATE TABLE IF NOT EXISTS w_text_small (
  tenant_id VARCHAR(32) NOT NULL,
  id        BIGINT      NOT NULL,
  status    TINYINT UNSIGNED NOT NULL,
  memo      TEXT        NOT NULL,
  PRIMARY KEY (tenant_id, id)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC;

-- 大きい 24KB を TEXT で持つ。行に収まらないので本体は行の外へ（off-page）。行にはポインタだけ
CREATE TABLE IF NOT EXISTS w_text_big (
  tenant_id VARCHAR(32) NOT NULL,
  id        BIGINT      NOT NULL,
  status    TINYINT UNSIGNED NOT NULL,
  memo      TEXT        NOT NULL,
  PRIMARY KEY (tenant_id, id)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC;

-- memo が無い基準（一番細い行）
CREATE TABLE IF NOT EXISTS w_narrow (
  tenant_id VARCHAR(32) NOT NULL,
  id        BIGINT      NOT NULL,
  status    TINYINT UNSIGNED NOT NULL,
  PRIMARY KEY (tenant_id, id)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC;
