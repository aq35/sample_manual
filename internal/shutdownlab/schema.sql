-- EXP-3: graceful shutdown の実験で使う表。
-- item_id を主キーにしておくことで、二重処理は DB が弾く（＝重複を数えられる）。
CREATE TABLE IF NOT EXISTS shutdown_item (
  tenant_id  VARCHAR(32) NOT NULL,
  item_id    INT         NOT NULL,
  worker     VARCHAR(64) NOT NULL,
  run        INT         NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, item_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS shutdown_lease (
  tenant_id  VARCHAR(32) NOT NULL,
  owner      VARCHAR(64) NOT NULL,
  renewed_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  renewals   INT         NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id)
) ENGINE=InnoDB;
