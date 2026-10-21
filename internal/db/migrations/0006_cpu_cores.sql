-- 0006_cpu_cores.sql —— CPU 口径补齐：把百分比翻译成「核」
--
-- 背景：主机级 cpu_pct 是**整机归一口径**（/proc/stat 汇总行，所有逻辑线程
-- 加总的忙碌占比，0–100），容器级 cpu_pct 是**单核口径**（与 docker stats
-- 同款，100% = 吃满一个核，可超过 100%）。两者量纲不同，同屏并列时
-- 「容器 100% 却比整机 50% 还高」是必然的误读。
--
-- 本迁移给两张快照表各加一列「核数」口径：
--   metrics_snapshots.busy_cores  = cpu_pct/100 × cores（占用几个核）
--   container_snapshots.cpu_cores = cpu_pct/100          （占用几个核）
-- 两者同量纲，可直接对照。**不改动原有 cpu_pct 列的语义**——历史行连续，
-- 图表不出现台阶式跳变。
--
-- 新列可空：旧行留 NULL（= 当时无此口径），读取方按「未知」处理。

ALTER TABLE metrics_snapshots ADD COLUMN busy_cores REAL;
ALTER TABLE container_snapshots ADD COLUMN cpu_cores REAL;
