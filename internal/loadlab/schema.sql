-- EXP-4: backpressure と過負荷の実験で使う表。
-- 現在状態は1キー1行（追記型にしない。調査 §4.4）。
CREATE TABLE IF NOT EXISTS load_item (
  tenant_id  VARCHAR(32) NOT NULL,
  k          VARCHAR(32) NOT NULL,
  v          BIGINT      NOT NULL,
  seq        BIGINT      NOT NULL,
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, k)
) ENGINE=InnoDB;
