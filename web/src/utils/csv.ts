/*
 * CSV 序列化（S7.7 审计导出用）。纯函数，不碰 DOM——下载触发在页面层。
 *
 * 两条规则动前先读：
 * 1. **转义按 RFC 4180**：字段含逗号、双引号或换行时，整个字段包双引号，内部
 *    双引号翻倍（""）。审计的 target/detail/error 都是自由文本，不做转义等于
 *    产出一个列数随数据漂移的废文件。
 * 2. **带 UTF-8 BOM**（\uFEFF）：Excel 认 BOM 才按 UTF-8 打开中文，否则运维
 *    拿 Excel 打开就是乱码（VS Code 等工具不受影响，带了也不亏）。
 */
import type { AuditLog } from '@/api/audit'

/** 单格转义：必要才包引号（无特殊字符时保持原样，文件更可读）。 */
export function csvCell(v: string | number): string {
  const s = String(v)
  if (/[",\r\n]/.test(s)) {
    return `"${s.replace(/"/g, '""')}"`
  }
  return s
}

/** CSV 列序：与页面表格列对应，另加 uid/req（排障要的两列）。 */
const COLUMNS: { head: string; get: (l: AuditLog) => string | number }[] = [
  { head: 'id', get: (l) => l.id },
  { head: 'ts', get: (l) => l.ts },
  { head: 'user_id', get: (l) => l.userId },
  { head: 'username', get: (l) => l.username },
  // server_id=0 原样导出（与具体服务器无关的动作）——CSV 是给工具消费的，不做 UI 层的「—」替换。
  { head: 'server_id', get: (l) => l.serverId },
  { head: 'action', get: (l) => l.action },
  { head: 'target', get: (l) => l.target },
  { head: 'result', get: (l) => l.result },
  { head: 'error', get: (l) => l.error },
  { head: 'detail', get: (l) => l.detail },
  { head: 'ip', get: (l) => l.ip },
  { head: 'request_id', get: (l) => l.requestId },
]

/**
 * 审计行 → CSV 文本（含表头与 BOM）。
 * 导出的是「当前已加载的行」：游标分页下没翻到的页不在文件里，页面须明示。
 */
export function auditLogsToCsv(logs: AuditLog[]): string {
  const lines = [COLUMNS.map((c) => csvCell(c.head)).join(',')]
  for (const l of logs) {
    lines.push(COLUMNS.map((c) => csvCell(c.get(l))).join(','))
  }
  return `\uFEFF${lines.join('\r\n')}\r\n`
}
