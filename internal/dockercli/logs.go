// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 容器日志（S3.3）。Engine 的日志接口有两条出参形态：
//   - TTY 容器：原始字节流，直接就是终端输出；
//   - 非 TTY 容器：8 字节帧头复用流（[流类型,0,0,0,长度BE4]），stdout/stderr 分帧。
// 面板要的是「合流文本」，所以 Demux 是默认路径；export（下载原始流）除外。

package dockercli

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// LogsOptions 是日志查询参数的类型别名，真定义在 internal/protocol：
// 查询串由 Hub 拼、Agent 解析，两侧共用同一份字段与合法值规则。
type LogsOptions = protocol.LogsOptions

// frameHeaderLen 是复用流帧头长度：1 字节流类型 + 3 填充 + 4 字节长度。
const frameHeaderLen = 8

// Logs 打开日志流。返回的 ReadCloser 是 Engine 原始流（可能复用、可能 TTY 裸流），
// 由调用方选择 Demux 或原样导出；关闭责任在调用方。
func (c *Client) Logs(ctx context.Context, id string, o LogsOptions) (io.ReadCloser, error) {
	if !idSafe(id) {
		return nil, fmt.Errorf("dockercli: 非法容器标识 %q", id)
	}
	q := "?stdout=1&stderr=1" // 合流：两类都要， Engine 约定缺省即不输出
	if o.Follow {
		q += "&follow=1"
	}
	if o.Tail != "" {
		q += "&tail=" + o.Tail
	}
	if o.Since != "" {
		q += "&since=" + o.Since
	}
	if o.Until != "" {
		q += "&until=" + o.Until
	}
	if o.Timestamps {
		q += "&timestamps=1"
	}
	resp, err := c.doStream(ctx, http.MethodGet, "/containers/"+id+"/logs"+q, o.Follow)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("dockercli: 容器 %s 不存在 (404)", id)
		}
		return nil, fmt.Errorf("dockercli: logs %s 返回 %d: %.200s", id, resp.StatusCode, body)
	}
	return resp.Body, nil
}

// doStream 与 do 的区别：不读干响应体（流式透传），超时可豁免（follow 用）。
func (c *Client) doStream(ctx context.Context, method, path string, noTimeout bool) (*http.Response, error) {
	if c.RequestTimeout > 0 && !noTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.RequestTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		return nil, fmt.Errorf("dockercli: 构造请求失败: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dockercli: 引擎请求失败: %w", err)
	}
	return resp, nil
}

// Demux 把复用流拆帧并把 stdout/stderr 载荷按序写入 w（合流）。
//
// TTY 裸流的判定：第一个字节不像流类型（0/1/2）时按裸流原样拷贝——
// 这是 docker CLI 同款启发式，比「让调用方先查 TTY 配置」少一次 inspect。
func Demux(r io.Reader, w io.Writer) error {
	header := make([]byte, frameHeaderLen)
	n, err := io.ReadFull(r, header)
	if err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			// 不足一帧头：只能是 TTY 裸流或空流，原样吐回。
			_, werr := w.Write(header[:n])
			if werr != nil {
				return werr
			}
			_, cerr := io.Copy(w, r)
			return cerr
		}
		return err
	}
	if header[0] > 2 || !plausibleSize(binary.BigEndian.Uint32(header[4:8])) {
		// 裸流：头 8 字节连同后续全部原样写回。
		if _, werr := w.Write(header); werr != nil {
			return werr
		}
		_, cerr := io.Copy(w, r)
		return cerr
	}
	// 复用流：循环拆帧，stdout(0)/stderr(1) 都写进同一个 w。
	payload := make([]byte, 64*1024)
	for {
		size := binary.BigEndian.Uint32(header[4:8])
		if size > 16<<20 {
			return fmt.Errorf("dockercli: 日志帧异常过大 (%d 字节)", size)
		}
		if int(size) > len(payload) {
			payload = make([]byte, size)
		}
		if _, err := io.ReadFull(r, payload[:size]); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("dockercli: 读帧载荷失败: %w", err)
		}
		if header[0] == 0 || header[0] == 1 { // stdout / stderr，合流
			if _, werr := w.Write(payload[:size]); werr != nil {
				return werr
			}
		} // 2=system 而 3+ 未定义：丢弃但继续读，不中断整条流
		if _, err := io.ReadFull(r, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil // 引擎关闭即正常结束
			}
			return err
		}
	}
}

// plausibleSize 粗判帧长字段是否像样：TTY 裸流的第 5-8 字节几乎不可能
// 恰好是 0 < n ≤ 16MB 且前 4 字节全零。误判的代价只是把复用流当裸流显示。
func plausibleSize(n uint32) bool { return n > 0 && n <= 16<<20 }
