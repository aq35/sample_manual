-- 一覧を「登録が新しい順」で出す画面が増えたので、その索引を足す。
--
-- ★索引を足すと、既存の問い合わせの実行計画が変わることがある（本当に起きた。
--   docs/repository-layer.md の 3.6 参照）。足したら実行計画のテストを回すこと。
CREATE INDEX idx_profile_created ON robot_profile (tenant_id, created_at);
