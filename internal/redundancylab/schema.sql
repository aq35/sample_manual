-- 冗長化（複数レプリカ）実験用のキュー。pending を各レプリカが取り合う。
CREATE TABLE IF NOT EXISTS redq (
  tenant_id VARCHAR(32) NOT NULL,
  id        BIGINT      NOT NULL,
  state     ENUM('pending','claimed') NOT NULL DEFAULT 'pending',
  owner     VARCHAR(64) NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, id),
  KEY pend (tenant_id, state, id)
) ENGINE=InnoDB;

-- fence（世代番号）で「古い担当の書き込み」を弾くための状態行。
CREATE TABLE IF NOT EXISTS redstate (
  tenant_id VARCHAR(32) NOT NULL,
  robot_id  VARCHAR(32) NOT NULL,
  val       VARCHAR(64) NOT NULL,
  fence     BIGINT      NOT NULL,
  PRIMARY KEY (tenant_id, robot_id)
) ENGINE=InnoDB;
