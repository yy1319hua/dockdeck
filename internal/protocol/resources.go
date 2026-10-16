// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

// 本文件是 Docker 卷与网络（S7.9）的只读 DTO。
//
// 三条与既有模块对齐的口径：
//
//  1. 字段名保留 Engine 原生名（与 ContainerSummary / ImageSummary 同口径），
//     不在本层重命名——「不猜 Docker 语义」红线，前端自己映射；
//  2. 卷的 Mountpoint 刻意不进 DTO：那是宿主机绝对路径，与文件模块
//     「对外不回宿主机绝对路径」同口径。不解析就不存在，也就无从泄漏
//     （放进 DTO 就得靠每个调用方记得不展示，那是留着出事）；
//  3. 两个端点的响应形态不同——/volumes 是对象包数组、/networks 是裸数组，
//     故分开两个类型，不套同一模板（S4.1 实测就抓到过这类形态差异）。
//
// 字段以 node1 只读实测为准（S7.9 Q4）：实测前只纳入已确认的部分，
// 未确认字段先不进 DTO，等实测回填再补——宁可少一个字段，不可错一个语义。

// VolumeUsageData 是卷的使用情况统计。
type VolumeUsageData struct {
	// Size 是卷占用字节数。Engine 对未统计的卷给 -1，前端按此显示「—」。
	Size int64 `json:"Size"`
	// RefCount 是使用该卷的容器数量。
	RefCount int64 `json:"RefCount"`
}

// VolumeSummary 是 GET /volumes 返回的一条卷记录。
type VolumeSummary struct {
	Name string `json:"Name"`
	// Driver 是卷驱动（local / nfs 等）。
	Driver string `json:"Driver"`
	// Scope 是卷作用域（local / global）。
	Scope   string            `json:"Scope"`
	Labels  map[string]string `json:"Labels"`
	Options map[string]string `json:"Options"`
	// CreatedAt 是 RFC3339 文本。Engine 给的是带纳秒与偏移的串，
	// 不保证 Z 后缀——前端不要按秒级字典序比较（审计页那条坑）。
	CreatedAt string `json:"CreatedAt"`
	// UsageData 可能为 null（部分驱动、或 Engine 未做统计时），故用指针：
	// nil 表示「Engine 没给」，UI 显示「—」。不能退化成零值——
	// 把「没统计」显示成「0 个容器在用」会让用户误判卷可删。
	UsageData *VolumeUsageData `json:"UsageData"`
}

// VolumesList 是 GET /volumes 的响应体：**对象包数组**。
// 与 Networks 的裸数组形态不同，两处解码模板不通用。
type VolumesList struct {
	Volumes []VolumeSummary `json:"Volumes"`
	// Warnings 是 Engine 的告警文本；无告警时为 null（解码得 nil，无需特判）。
	Warnings []string `json:"Warnings"`
}

// NetworkContainer 是接入某个网络的容器条目。
// GET /networks 的清单里就带这个映射，故「谁接入了这个网络」零额外请求——
// 与卷相反：卷只给 RefCount 计数，不给名单（要看名单须逐个容器 inspect，
// 而 ContainerSummary 目前没有 Mounts 字段，补它要动三层，故不做）。
type NetworkContainer struct {
	Name        string `json:"Name"`
	EndpointID  string `json:"EndpointID"`
	MacAddress  string `json:"MacAddress"`
	IPv4Address string `json:"IPv4Address"`
	IPv6Address string `json:"IPv6Address"`
}

// NetworkIPAMConfig 是网络 IPAM 的一段配置。
type NetworkIPAMConfig struct {
	Subnet  string `json:"Subnet"`
	IPRange string `json:"IPRange"`
	Gateway string `json:"Gateway"`
}

// NetworkIPAM 是网络的 IP 地址管理段。
type NetworkIPAM struct {
	Driver string              `json:"Driver"`
	Config []NetworkIPAMConfig `json:"Config"`
}

// NetworkSummary 是 GET /networks 返回的一条网络记录。
type NetworkSummary struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Scope      string            `json:"Scope"`
	Created    string            `json:"Created"`
	Internal   bool              `json:"Internal"`
	Attachable bool              `json:"Attachable"`
	Ingress    bool              `json:"Ingress"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	Labels     map[string]string `json:"Labels"`
	IPAM       NetworkIPAM       `json:"IPAM"`
	// Containers 的键是容器 ID。无容器接入时 Engine 给 null 或空对象，
	// 两种都接得住：解码 null 得 nil map，range 零次迭代，无需特判。
	Containers map[string]NetworkContainer `json:"Containers"`
}
