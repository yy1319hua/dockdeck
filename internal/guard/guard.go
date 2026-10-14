// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package guard 是「现有容器与数据保护」三重门的实现载体
// （ARCHITECTURE「现有容器与数据保护」第 3/4 条）：
//
//   - 保护名单命中即拒：名单是正则，存 settings.protected_containers，
//     Hub 侧在转发破坏性动作前校验；
//   - 不碰自身：Agent 守物理防线，针对自身容器的写操作一律 403；
//   - 破坏性确认：删除容器要求调用方回传目标名字，confirm 与目标
//     完全一致才放行。
//
// 职责分工：Hub 守名单与 confirm（有 settings、有 UI 语义）；Agent 只守
// 自身（名单属于 Hub 的运维知识，Agent 保持无状态）。两侧不共享实例，
// 但共享本包的比较语义，杜绝各写一份导致漂移。
package guard

import (
	"fmt"
	"regexp"
	"strings"
)

// SettingKey 是 settings 表里保护名单的键名。值为逗号分隔的正则。
const SettingKey = "protected_containers"

// DefaultPatterns 是保护名单缺省值。
//
// 只放 DockDeck 自身前缀一条。ARCHITECTURE 定稿时曾拟默认含 watchtower，
// 2026-10-15 用户明确：watchtower/komari 由 TA 部署后自行删除，不进保护
// 名单——默认名单因此收窄；存量容器名由部署时录入 settings 追加。
var DefaultPatterns = []string{"^dockdeck-"}

// Guard 聚合两类守卫：自身识别与保护名单。
type Guard struct {
	selfID   string
	patterns []*regexp.Regexp
}

// New 构造。selfID 空表示本侧无自身可保护（Hub 没有容器身份）；
// 正则编译失败返回错误——名单是安全边界，坏正则宁可拒绝启动/拒绝请求，
// 也不静默降级为放行。
func New(selfID string, patterns ...string) (*Guard, error) {
	g := &Guard{selfID: strings.ToLower(strings.TrimSpace(selfID))}
	for _, p := range patterns {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("guard: 保护名单正则 %q 编译失败: %w", p, err)
		}
		g.patterns = append(g.patterns, re)
	}
	return g, nil
}

// IsSelf 判断目标是否命中自身。比较是大小写不敏感的双向前缀匹配：
// 容器 ID 的 12 位短形式与 64 位全形式、名字都覆盖到。语义承接
// cmd/agent 里原先的 containersAPI.isSelf（S3.2），行为不变。
func (g *Guard) IsSelf(target string) bool {
	if g == nil || g.selfID == "" || target == "" {
		return false
	}
	t := strings.ToLower(target)
	return strings.HasPrefix(t, g.selfID) || strings.HasPrefix(g.selfID, t)
}

// IsProtected 报告目标是否命中保护名单。空名单永不命中——「默认全拒」
// 会让空名单配置退化成面板不可用，运维语义应为「名单即例外清单」。
func (g *Guard) IsProtected(name string) bool {
	if g == nil || name == "" {
		return false
	}
	for _, re := range g.patterns {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// Confirm 破坏性动作的名字确认：confirm 与目标完全一致（去首尾空白）才
// 放行。空 confirm 视为未确认，错误文案给调用方直接透出。
func Confirm(target, confirm string) error {
	if strings.TrimSpace(confirm) != target {
		return fmt.Errorf("名字确认不符：此为破坏性操作，须回传目标名 %q 以确认", target)
	}
	return nil
}

// ParseList 把 settings 里逗号分隔的正则串拆成切片（去空段、去首尾空白）。
func ParseList(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
