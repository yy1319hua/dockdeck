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
	"net/url"

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
		// QueryEscape 是防御性的：Agent 侧 logsQuery 已归一成 Unix 秒
		// （纯数字，转义前后一致），但库不把正确性寄托在调用方身上——
		// 万一有别的调用方递 RFC3339 进来，"+08:00" 也不会被解析成空格。
		q += "&since=" + url.QueryEscape(o.Since)
	}
	if o.Until != "" {
		q += "&until=" + url.QueryEscape(o.Until)
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
	var cancel context.CancelFunc
	if c.RequestTimeout > 0 && !noTimeout {
		ctx, cancel = context.WithTimeout(ctx, c.RequestTimeout)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("dockercli: 构造请求失败: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("dockercli: 引擎请求失败: %w", err)
	}
	if cancel != nil {
		// 超时 ctx 必须活到 Body 关闭：这是流式响应，由调用方随后读取。
		// 若在返回前 cancel（原 defer cancel 写法），Body 立刻失效，
		// 容器日志非 follow 模式会整条读成 0 字节（GAP-8，node1 实测）。
		resp.Body = &bodyWithCancel{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
}

// bodyWithCancel 把 ctx 的 cancel 推迟到 Body 关闭时执行。
// 正常读完并 Close 释放计时器；调用方中途放弃时 Close 同样会取消，不漏。
type bodyWithCancel struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *bodyWithCancel) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
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
		// Docker 复用流的类型定义：0=stdin、1=stdout、2=stderr。
		// 2 是 stderr，不是「system」——早期把它当未定义类型丢弃，
		// 导致所有写 stderr 的程序（Go 的 log、logrus 默认输出）日志整条为空
		// （GAP-9，node1 实测：dockdeck-hub/watchtower 经面板看是 0 字节）。
		// 3+ 才是未定义：丢弃但继续读，不中断整条流。
		if header[0] <= 2 {
			if _, werr := w.Write(payload[:size]); werr != nil {
				return werr
			}
		}
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
