// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 镜像拉取（S4.1 中）：POST /images/create。
//
// 与其余写操作的两个差别，也是本文件单独落盘的理由：
//   - 长操作：拉大镜像以分钟计，默认 10s 请求超时必掐，超时单独放宽到
//     PullTimeout（其余写操作维持 5s 短超时——那是 agentcli 层的口径，
//     这里说的是 dockercli→Engine 这一跳）；
//   - 流式响应：Engine 回 NDJSON 进度流（每行一个事件），远超 do() 的
//     1MB 读上限，必须走 doStream 边收边汇总。
//
// 呈现取「汇总式」（方案决策点 1）：读完整流，把最终状态/层清单/错误
// 打包成 PullResult 返回。实时进度透传留到 S7 前端段——那时才值得为它
// 把流穿透三层。

package dockercli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// PullTimeout 是 pull 的超时。选 10 分钟：大镜像（>1GB）经内网 registry
// 拉取的现实量级；再长就该用任务化拉取（S7+ 的事），不该让 HTTP 请求
// 无限等。
const PullTimeout = 10 * time.Minute

// PullResult 是一次 pull 的汇总结果。
type PullResult struct {
	// Status 是 Engine 回报的最终状态串（如
	// "Status: Downloaded newer image for alpine:3.20"），原样透传。
	Status string
	// Layers 是流里出现过的层 ID（去重保序）——「拉了哪些层」的账单。
	Layers []string
	// Err 是 Engine 在流里回报的错误文本（error 字段非空的最后一条）。
	// 非空即拉取失败，即便 HTTP 状态是 200——Engine 的这个接口把
	// 业务错误放在流里而非状态码里。
	Err string
}

// pullEvent 是进度流里的一条。Engine 的事件字段远不止这些
// （progressDetail/aux/stream...），只取汇总需要的三个。
type pullEvent struct {
	Status string `json:"status"`
	ID     string `json:"id"`
	Error  string `json:"error"`
}

// Pull 拉取镜像。ref 形如 alpine / alpine:3.20 / registry:5000/app:v1 /
// app@sha256:<hex>；先过 ValidImageRef（防注入），再拆 name/tag——
// Engine 的 fromImage 与 tag 是两个参数，合起来传重复信息它会 400。
func (c *Client) Pull(ctx context.Context, ref string) (*PullResult, error) {
	if !protocol.ValidImageRef(ref) {
		return nil, fmt.Errorf("dockercli: 非法镜像引用 %q", ref)
	}
	fromImage, tag := splitImageRef(ref)
	q := url.Values{}
	q.Set("fromImage", fromImage)
	if tag != "" {
		q.Set("tag", tag)
	}
	// pull 的超时在此单独接管：doStream 的 noTimeout=true 豁免默认 10s，
	// 由本层 10 分钟兜底。调用方 ctx 若自带更短截止，以更短者为准
	// （context.WithTimeout 天然取最早截止）。
	ctx, cancel := context.WithTimeout(ctx, PullTimeout)
	defer cancel()
	resp, err := c.doStream(ctx, http.MethodPost, "/images/create?"+q.Encode(), true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("dockercli: pull %s 返回 %d: %.200s", ref, resp.StatusCode, body)
	}
	return readPullStream(resp.Body)
}

// splitImageRef 把镜像引用拆成 Engine 要的 (fromImage, tag)。
//
// 拆分规则（不猜语义的务实子集）：
//   - 含 "@"：digest 形态，整体作 fromImage（digest 不是 tag）；
//   - 最后一个 ":" 后不含 "/"：是 name:tag（registry 端口在路径前，
//     "5000/app" 里的冒号后带 "/"，不会被误判成 tag 分隔符）；
//   - 其余：整体作 fromImage，tag 缺省（Engine 用 latest）。
func splitImageRef(ref string) (fromImage, tag string) {
	if strings.ContainsRune(ref, '@') {
		return ref, ""
	}
	i := strings.LastIndexByte(ref, ':')
	if i < 0 || strings.ContainsRune(ref[i+1:], '/') {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

// readPullStream 逐行读 NDJSON 进度流并汇总。
// 行上限放到 1MB（默认 64KB）：progress 字段带全角进度条字符时可能很长。
func readPullStream(r io.Reader) (*PullResult, error) {
	// Layers 预置空 slice：与 list 同口径，空清单编码为 [] 非 null。
	res := &PullResult{Layers: []string{}}
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev pullEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			// 单行坏 JSON 不中断整条流：Engine 偶尔混入非事件行，
			// 汇总式呈现下丢一行进度无伤大雅，拉取结果以最终状态为准。
			continue
		}
		if ev.Error != "" {
			res.Err = ev.Error // 保留最后一条错误
			continue
		}
		if ev.Status != "" {
			res.Status = ev.Status
		}
		// 层 ID 的判定按形态：Docker 镜像层就是 hex digest（pull 流里
		// 是完整 64 位）。首条 "Pulling from" 事件的 id 是 tag 名
		// （如 "3.20"），不是层——按形态过滤把两类分开，不靠 status 文案猜。
		if ev.ID != "" && isHexID(ev.ID) && !seen[ev.ID] {
			seen[ev.ID] = true
			res.Layers = append(res.Layers, ev.ID)
		}
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("dockercli: pull 进度流读取失败: %w", err)
	}
	return res, nil
}

// isHexID 判断是否为层摘要形态：长度 ≥8 的纯 hex。
// 8 是下限经验值——pull 流里层 ID 总是完整 64 位，留余量防短 ID 变体。
func isHexID(s string) bool {
	if len(s) < 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
