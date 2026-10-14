// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import "context"

// Version 对应 GET /version 的响应。只取面板用得到的字段，多余字段忽略——
// Engine 的返回一直在膨胀，按需声明可避免「新字段导致解码失败」这类脆断。
type Version struct {
	Version       string `json:"Version"`       // Docker 版本，如 29.1.3
	APIVersion    string `json:"ApiVersion"`    // API 版本
	MinAPIVersion string `json:"MinAPIVersion"` // 服务器仍兼容的最老 API 版本
	GitCommit     string `json:"GitCommit"`
	GoVersion     string `json:"GoVersion"`
	Os            string `json:"Os"`
	Arch          string `json:"Arch"`
	KernelVersion string `json:"KernelVersion"`
	BuildTime     string `json:"BuildTime"`
	Experimental  bool   `json:"Experimental"`
}

// SystemInfo 对应 GET /info 的响应，同样按需取字段。
// 容器/镜像计数与资源规模是「服务器概览」的核心，其余运维字段暂不进结构。
type SystemInfo struct {
	ID                string `json:"ID"`
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	ContainersPaused  int    `json:"ContainersPaused"`
	ContainersStopped int    `json:"ContainersStopped"`
	Images            int    `json:"Images"`
	Name              string `json:"Name"`
	ServerVersion     string `json:"ServerVersion"`
	OperatingSystem   string `json:"OperatingSystem"`
	OSType            string `json:"OSType"`
	Architecture      string `json:"Architecture"`
	NCPU              int    `json:"NCPU"`
	MemTotal          int64  `json:"MemTotal"`
	DockerRootDir     string `json:"DockerRootDir"`
}

// Version 拉取引擎版本信息（GET /version，走协商出的版本前缀）。
func (c *Client) Version(ctx context.Context) (*Version, error) {
	v := &Version{}
	if err := c.getJSON(ctx, "/version", v); err != nil {
		return nil, err
	}
	return v, nil
}

// Info 拉取系统概览（GET /info）。
func (c *Client) Info(ctx context.Context) (*SystemInfo, error) {
	i := &SystemInfo{}
	if err := c.getJSON(ctx, "/info", i); err != nil {
		return nil, err
	}
	return i, nil
}
