// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package diff 提供纯文本的 unified diff 计算（类 `diff -u`），零外部依赖。
// 用于 Compose YAML 写前预览：把"目标内容相对当前文件"的差异算出来展示给用户，
// 不落盘、也不产生备份——真正的写入由调用方在用户确认后进行。
package diff

import (
	"fmt"
	"strings"
)

// Unified 计算 oldText 与 newText 的 unified diff；两文本相同（或经行尾归一后相同）
// 返回空串。算法为 LCS 回溯 + 上下文窗口分块，与 GNU diff -u 的 hunk 头语义一致
// （纯新增时旧区间 `-0,0`、纯删除时新区间 `+0,0`）。
func Unified(oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	a := splitLines(oldText)
	b := splitLines(newText)
	n, m := len(a), len(b)

	// dp[i][j] = a[i:] 与 b[j:] 的 LCS 长度。
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	// 回溯成编辑序列：' ' 相等 / '-' 删除 / '+' 新增。
	type op struct {
		kind rune
		text string
	}
	var seq []op
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			seq = append(seq, op{' ', a[i]})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			seq = append(seq, op{'-', a[i]})
			i++
		} else {
			seq = append(seq, op{'+', b[j]})
			j++
		}
	}
	for i < n {
		seq = append(seq, op{'-', a[i]})
		i++
	}
	for j < m {
		seq = append(seq, op{'+', b[j]})
		j++
	}

	const ctx = 3
	// in[k] = 第 k 个 seq 位置是否落入变更区（含前后 ctx 行上下文）。
	// 上下文窗口重叠的相邻变更会被自然地合并到同一 hunk。
	in := make([]bool, len(seq))
	for k := 0; k < len(seq); k++ {
		if seq[k].kind == ' ' {
			continue
		}
		for d := -ctx; d <= ctx; d++ {
			if kk := k + d; kk >= 0 && kk < len(seq) {
				in[kk] = true
			}
		}
	}

	var sb strings.Builder
	oi, ni := 0, 0 // 已推过的 old/new 行数（用于 @@ 头 1-based 定位）
	k := 0
	for k < len(seq) {
		if !in[k] {
			switch seq[k].kind {
			case ' ':
				oi++
				ni++
			case '-':
				oi++
			case '+':
				ni++
			}
			k++
			continue
		}
		oldBefore, newBefore := oi, ni
		oldCount, newCount := 0, 0
		var lines []string
		for k < len(seq) && in[k] {
			switch seq[k].kind {
			case ' ':
				lines = append(lines, " "+seq[k].text)
				oldCount++
				newCount++
				oi++
				ni++
			case '-':
				lines = append(lines, "- "+seq[k].text)
				oldCount++
				oi++
			case '+':
				lines = append(lines, "+ "+seq[k].text)
				newCount++
				ni++
			}
			k++
		}
		// @@ 头：纯新增时旧区间从 0 起，纯删除时新区间从 0 起（GNU 语义）。
		oldStart, newStart := oldBefore, newBefore
		if oldCount > 0 {
			oldStart = oldBefore + 1
		}
		if newCount > 0 {
			newStart = newBefore + 1
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, l := range lines {
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// splitLines 按行切分，先去掉统一尾部换行再切，避免"内容相同但结尾换行"产生噪声 diff。
func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
