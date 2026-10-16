// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import (
	"reflect"
	"testing"
)

// TestVolumeSummaryNoMountpoint 是红线回归：卷的 Mountpoint 是宿主机绝对路径，
// 与文件管理模块「对外不回宿主机绝对路径」同口径，故不进 DTO。
//
// 放进 DTO 就得靠每个调用方记得不展示——那是留着出事；从类型上消掉，
// 谁也漏不出来。将来若真要展示，应当显式加回并写明取舍，而不是悄悄补字段。
func TestVolumeSummaryNoMountpoint(t *testing.T) {
	tp := reflect.TypeOf(VolumeSummary{})
	if _, ok := tp.FieldByName("Mountpoint"); ok {
		t.Error("VolumeSummary 不该有 Mountpoint 字段（宿主机绝对路径不外泄，见文件模块同口径）")
	}

	// 顺带钉住 UsageData 是指针：退化成值类型会把 JSON 的 null 读成 0，
	// 「Engine 没统计」就被当成「0 个容器在用」，用户据此误判卷可删。
	f, ok := tp.FieldByName("UsageData")
	if !ok {
		t.Fatal("VolumeSummary 缺 UsageData 字段")
	}
	if f.Type.Kind() != reflect.Ptr {
		t.Errorf("UsageData 应为指针（null 要能表示成 nil），got %s", f.Type.Kind())
	}
}

// TestNetworkContainersNullable 钉住网络清单的 Containers 可以是空映射：
// 无容器接入时 Engine 给 null 或 {}，两者都不得让解码报错，
// 且解码结果必须是「零次迭代」而非 panic（前端直接 range）。
func TestNetworkContainersNullable(t *testing.T) {
	tp := reflect.TypeOf(NetworkSummary{})
	f, ok := tp.FieldByName("Containers")
	if !ok {
		t.Fatal("NetworkSummary 缺 Containers 字段")
	}
	if f.Type.Kind() != reflect.Map {
		t.Errorf("Containers 应为 map（键是容器 ID），got %s", f.Type.Kind())
	}
}
