-- EXP-1: 外部 effect と crash の実験で使う表。
--
-- 設計の要点
--   * 状態は「うまくいったか」ではなく **こちらが何を知っているか** を表す。
--     COMPLETED は「相手が発行した受領書を持っている」ことを意味する
--   * receipt_id には **相手が発行した ID しか入れない**。
--     エラー文字列を受領書として保存するのは禁止（何も証明していないため）
--   * last_error は記録として残すが、状態遷移の根拠にはしない

CREATE TABLE IF NOT EXISTS effect_request (
  tenant_id      VARCHAR(32) NOT NULL,
  request_id     VARCHAR(64) NOT NULL,
  amount         INT         NOT NULL,
  idem_key       VARCHAR(64) NOT NULL,
  -- NOT_STARTED / DISPATCH_RESERVED / OUTCOME_UNKNOWN /
  -- CLAIMED_SUCCESS / INDEPENDENTLY_OBSERVED / COMPLETED
  state          VARCHAR(32) NOT NULL,
  attempts       INT         NOT NULL DEFAULT 0,
  receipt_id     VARCHAR(64) NULL,
  receipt_source VARCHAR(16) NULL,   -- response / observation
  last_error     VARCHAR(255) NULL,
  created_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, request_id)
) ENGINE=InnoDB;

-- outbox 方式で使う。業務状態の保存と「送るつもり」の記録を同じトランザクションに入れる。
CREATE TABLE IF NOT EXISTS effect_outbox (
  tenant_id   VARCHAR(32) NOT NULL,
  request_id  VARCHAR(64) NOT NULL,
  state       VARCHAR(32) NOT NULL,   -- PENDING / RESERVED / DONE
  reserved_by VARCHAR(64) NULL,
  reserved_at DATETIME(3) NULL,
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, request_id)
) ENGINE=InnoDB;
