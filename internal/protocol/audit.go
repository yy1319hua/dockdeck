// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

// AuditLog 是 audit_logs 表的一行（查询返回用，S7.7）。
//
// 字段与表一一对应（迁移 0001，server_id/user_id 可空——用户或服务器被删后
// 审计行仍在，ON DELETE SET NULL；查询侧 COALESCE 成 0/空串，前端不需要
// 处理 null）。时间 ts 是 RFC3339 UTC 字符串（表 DEFAULT 即此格式）。
type AuditLog struct {
	ID        int64  `json:"id"`
	Ts        string `json:"ts"`
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	ServerID  int64  `json:"server_id"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Detail    string `json:"detail"`
	Result    string `json:"result"` // ok | error
	Error     string `json:"error"`
	IP        string `json:"ip"`
	RequestID string `json:"request_id"`
}

// AuditLogsPage 是审计查询结果：倒序（最新在前）+ before_id 游标分页。
type AuditLogsPage struct {
	Logs []AuditLog `json:"logs"`
	// NextBefore 是「加载更多」的游标——作为下一次请求的 before_id；
	// 0 表示没有更多。游标按 id 锚定而非 offset：审计是 append-only 表，
	// 翻页期间新写入的行只会出现在更靠前的页，不会插进已翻过的页中间
	// （offset 分页在这种表上会重复/漏行，方案 Q1 选游标的原因）。
	NextBefore int64 `json:"next_before"`
}
