-- 「列を足して、その列に索引を張る」= 2文からなるマイグレーション。
--
-- ★1ファイル1変更が原則だが、実運用では「列の追加」と「有効化（索引・制約）」が
-- 対になることがある。MySQL の DDL は暗黙コミットするので、
-- **この2文の途中で落ちると、列はあるが索引が無い中途半端な形が残る。**
-- EXP-6 はその状態を実際に作り、次の起動が何をするかを確かめる。
ALTER TABLE robot_profile ADD COLUMN retired TINYINT(1) NOT NULL DEFAULT 0;

CREATE INDEX idx_profile_retired ON robot_profile (tenant_id, retired);
