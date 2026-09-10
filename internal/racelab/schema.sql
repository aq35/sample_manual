-- EXP-66: Web と Worker が同じ行を触るレースコンディションの実験。
-- 状態遷移は status の CAS（affected_rows で勝者確定）、入力の同時編集は version（楽観ロック）で守る。
CREATE TABLE IF NOT EXISTS race_job (
  tenant_id VARCHAR(32) NOT NULL,
  id        BIGINT      NOT NULL,
  status    ENUM('pending','in_progress','completed','cancelled') NOT NULL,
  owner     VARCHAR(32) NOT NULL DEFAULT '',
  input     BIGINT      NOT NULL DEFAULT 0,  -- Web が編集しうる入力
  version   BIGINT      NOT NULL DEFAULT 0,  -- 楽観ロック（Web の編集で +1）
  result    BIGINT          NULL,            -- worker が input から計算して書く結果
  PRIMARY KEY (tenant_id, id),
  KEY idx_status (tenant_id, status, id)
) ENGINE=InnoDB;
