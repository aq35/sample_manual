-- リポジトリ層が使う表。マルチテナントの基本形を1つに集めてある。
--
--   * 主キーは (tenant_id, ...)  … テナント単位の読み書きが隣接ページで済む（実験で 2.3 倍差）
--   * ランダムな id を主キーにしない … 実験で表サイズが約2倍、投入が 1.4 倍遅い
--   * version 列 … 「読んで書く」ときの更新消失を防ぐ（実験で 200 回中 174 回消えた）

CREATE TABLE IF NOT EXISTS robot_profile (
  tenant_id  VARCHAR(32)     NOT NULL,
  robot_id   VARCHAR(32)     NOT NULL,
  name       VARCHAR(64)     NOT NULL,
  model_name VARCHAR(64)     NOT NULL,
  serial     VARCHAR(64)     NOT NULL,
  -- version は楽観ロック用。更新のたびに +1 する
  version    BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, robot_id),
  -- テナント内で一意にしたいもの（serial）は、必ず tenant_id を先頭に含める。
  -- tenant_id を含めない一意索引は、他テナントの登録を弾いてしまう
  UNIQUE KEY uq_tenant_serial (tenant_id, serial)
) ENGINE=InnoDB;
