-- EXP-65: 通常動作では入らない不正データが混入して worker を止める、その「入口」の実験。
-- アプリのバリデーションは「アプリ経路」しか守れない。直接 DB 入力（手動 SQL・別ツール・移行スクリプト）は
-- それを素通りする。DB 制約だけが全経路の最後の砦になる。

-- 参照先の実在マスタ（FK 用）
CREATE TABLE IF NOT EXISTS ing_ref (
  id BIGINT NOT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB;

-- 入口ガードなし: 型がゆるく制約も無い。どんな値でも landing する（不正状態・負値・存在しない参照）。
-- アプリのバグや直接 DB 入力がそのまま poison として残り、後で worker を止める。
CREATE TABLE IF NOT EXISTS ing_open (
  tenant_id VARCHAR(32) NOT NULL,
  id        BIGINT      NOT NULL,
  status    VARCHAR(16) NOT NULL,  -- 自由文字列。'frozen' のような未知状態も入る
  amount    BIGINT      NOT NULL,  -- 負値も入る
  ref_id    BIGINT      NOT NULL,  -- 存在しない参照も入る
  PRIMARY KEY (tenant_id, id)
) ENGINE=InnoDB;

-- 入口ガードあり: DB 制約が最後の砦。直接 DB 入力でも不正は書けない。
--   ENUM   … 未知の状態を弾く
--   CHECK  … 負値を弾く（MySQL 8.0.16+ が enforce）
--   FK     … 存在しない参照を弾く
-- ※ ENUM / 範囲の「不正値を黙って丸めるのではなく弾く」には STRICT sql_mode が要る（MySQL 8 の既定で入る）。
CREATE TABLE IF NOT EXISTS ing_guarded (
  tenant_id VARCHAR(32) NOT NULL,
  id        BIGINT      NOT NULL,
  status    ENUM('pending','in_progress','completed') NOT NULL,
  amount    BIGINT      NOT NULL,
  ref_id    BIGINT      NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT chk_amount CHECK (amount >= 0),
  CONSTRAINT fk_ref FOREIGN KEY (ref_id) REFERENCES ing_ref(id)
) ENGINE=InnoDB;
