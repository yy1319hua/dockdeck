-- 0007_server_type.sql —— 服务器类型：区分「本机」与「远端 Agent」
--
-- 背景：Hub 自身所在机器不再需要单独部署 Agent——Hub 进程内起一个监听
-- loopback 的 agent 实例即可（复用 internal/agenthttp 全套路由），从而在
-- 服务器列表里以「本机」出现，与远端 Agent 走同一套 API 与采集链路。
--
-- 本迁移给 servers 表加一列 type：
--   'agent' —— 远端 Agent（默认；存量行全部属于此类）
--   'local' —— Hub 本机（由 Hub 启动时幂等注册，固定 server_id，不可删除）
--
-- 用 NOT NULL DEFAULT 'agent' 让存量行自动归类，且新 enroll 的远端 Agent
-- 无需显式写该列。类型只由 Hub 代码写入，不接收外部输入。

ALTER TABLE servers ADD COLUMN type TEXT NOT NULL DEFAULT 'agent';
