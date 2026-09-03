-- EXP-2: lease / fencing / clock skew の実験で使う表。

-- 担当（リース）。expires_at の判定は **DB の時計** で行う。
-- 各プロセスの自己申告時刻で判定すると、時計がずれたプロセスが担当を奪える。
CREATE TABLE IF NOT EXISTS fence_lease (
  tenant_id  VARCHAR(32)     NOT NULL,
  owner      VARCHAR(64)     NOT NULL,
  expires_at DATETIME(3)     NOT NULL,
  -- fence は担当が変わるたびに単調に増える番号。
  -- 「古い担当の書き込みを拒否する」ための唯一の根拠になる
  fence      BIGINT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id)
) ENGINE=InnoDB;

-- 担当だけが書ける状態。書き込みには fence を添える。
CREATE TABLE IF NOT EXISTS fence_state (
  tenant_id  VARCHAR(32)     NOT NULL,
  k          VARCHAR(64)     NOT NULL,
  v          BIGINT          NOT NULL,
  fence      BIGINT UNSIGNED NOT NULL,
  writer     VARCHAR(64)     NOT NULL,
  updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, k)
) ENGINE=InnoDB;

-- 監査用: 誰がいつ書いたか（受理・拒否の両方）。
CREATE TABLE IF NOT EXISTS fence_audit (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  tenant_id  VARCHAR(32)     NOT NULL,
  writer     VARCHAR(64)     NOT NULL,
  fence      BIGINT UNSIGNED NOT NULL,
  accepted   TINYINT(1)      NOT NULL,
  note       VARCHAR(64)     NOT NULL,
  at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_tenant (tenant_id, id)
) ENGINE=InnoDB;
