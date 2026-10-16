package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// YAML 把 spec 渲染为 YAML（OpenAPI 文档常以 YAML 分发，便于人读与 diff）。
//
// 零依赖实现：先把 Doc 编成 JSON（结构体按声明序、map 按字典序，字节稳定），
// 再用 json.Decoder 逐 token 解析成「保序值树」，最后按 YAML 规则缩进输出。
// 因此 YAML 与 JSON() 的键序逐节一致，两份产物可对照阅读。
func (d *Doc) YAML() ([]byte, error) {
	raw, err := d.JSON()
	if err != nil {
		return nil, err
	}
	val, err := decodeOrderedJSON(raw)
	if err != nil {
		return nil, err
	}
	root, ok := val.(orderedObject)
	if !ok {
		return nil, fmt.Errorf("spec 根不是对象")
	}
	var b strings.Builder
	if err := emitObject(&b, root, 0); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// yamlKV 是保序对象的单个键值对。
type yamlKV struct {
	Key string
	Val interface{}
}

// orderedObject 保留 JSON 里的键序（普通 map 会丢序，而 spec 的可读性依赖顺序）。
type orderedObject []yamlKV

func decodeOrderedJSON(data []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeOrderedValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil { // 尾部残留 token = 输入非单个 JSON 值
		return nil, fmt.Errorf("json 含多余内容")
	}
	return v, nil
}

func decodeOrderedValue(dec *json.Decoder) (interface{}, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil // string | json.Number | bool | nil
	}
	switch delim {
	case '{':
		obj := orderedObject{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := kt.(string)
			if !ok {
				return nil, fmt.Errorf("对象键不是字符串")
			}
			val, err := decodeOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			obj = append(obj, yamlKV{Key: key, Val: val})
		}
		if _, err := dec.Token(); err != nil { // 消费 "}"
			return nil, err
		}
		return obj, nil
	case '[':
		arr := []interface{}{}
		for dec.More() {
			val, err := decodeOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, val)
		}
		if _, err := dec.Token(); err != nil { // 消费 "]"
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("未知分隔符 %v", delim)
}

const indentUnit = "  "

func pad(level int) string { return strings.Repeat(indentUnit, level) }

// emitObject 写一个映射：每行 "key: <value>"，子集合换行缩进。
func emitObject(b *strings.Builder, obj orderedObject, level int) error {
	for _, kv := range obj {
		b.WriteString(pad(level))
		b.WriteString(yamlKey(kv.Key))
		b.WriteString(":")
		if err := emitAfterPrefix(b, kv.Val, level); err != nil {
			return err
		}
	}
	return nil
}

// emitSequence 写一个序列：每项行首 "-"，子集合换行缩进（"-" 独占一行）。
func emitSequence(b *strings.Builder, arr []interface{}, level int) error {
	for _, item := range arr {
		b.WriteString(pad(level))
		b.WriteString("-")
		if err := emitAfterPrefix(b, item, level); err != nil {
			return err
		}
	}
	return nil
}

// emitAfterPrefix 写 "key:" 或 "-" 之后的部分：标量接同行，集合换行后加一层缩进。
func emitAfterPrefix(b *strings.Builder, v interface{}, level int) error {
	switch val := v.(type) {
	case orderedObject:
		if len(val) == 0 {
			b.WriteString(" {}\n")
			return nil
		}
		b.WriteString("\n")
		return emitObject(b, val, level+1)
	case []interface{}:
		if len(val) == 0 {
			b.WriteString(" []\n")
			return nil
		}
		b.WriteString("\n")
		return emitSequence(b, val, level+1)
	case string:
		b.WriteString(" ")
		return emitString(b, val, level)
	default:
		b.WriteString(" ")
		return emitScalar(b, v)
	}
}

// emitString 写字符串：多行且各段安全时用字面块（|-），否则双引号转义（JSON 串即合法 YAML）。
func emitString(b *strings.Builder, s string, level int) error {
	if strings.Contains(s, "\n") && blockScalarSafe(s) {
		b.WriteString("|-\n")
		body := pad(level + 1)
		for _, line := range strings.Split(s, "\n") {
			if line == "" {
				b.WriteString("\n") // 空行不缩进，否则会被当成含空格的内容
				continue
			}
			b.WriteString(body)
			b.WriteString(line)
			b.WriteString("\n")
		}
		return nil
	}
	enc, err := json.Marshal(s)
	if err != nil {
		return err
	}
	b.Write(enc)
	b.WriteString("\n")
	return nil
}

// emitScalar 写非字符串标量（字符串在 emitString 里处理）。
func emitScalar(b *strings.Builder, v interface{}) error {
	switch val := v.(type) {
	case nil:
		b.WriteString("null\n")
	case bool:
		if val {
			b.WriteString("true\n")
		} else {
			b.WriteString("false\n")
		}
	case json.Number:
		b.WriteString(val.String())
		b.WriteString("\n")
	case string:
		enc, err := json.Marshal(val)
		if err != nil {
			return err
		}
		b.Write(enc)
		b.WriteString("\n")
	default:
		return fmt.Errorf("无法序列化的类型 %T", v)
	}
	return nil
}

// blockScalarSafe 判断多行字符串能否安全用字面块（不丢前导空格、不改变语义）。
// 任一行的首尾带空格都退到双引号——块标量对这些情形有折叠风险。
func blockScalarSafe(s string) bool {
	if strings.HasSuffix(s, "\n") || strings.HasSuffix(s, "\r") {
		return false
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			return false
		}
		if strings.TrimRight(line, " \t") != line {
			return false
		}
	}
	return true
}

// yamlKey 在安全时用裸键，否则双引号。
func yamlKey(k string) string {
	if isPlainKeySafe(k) {
		return k
	}
	enc, _ := json.Marshal(k)
	return string(enc)
}

// isPlainKeySafe 只承认 [A-Za-z_][A-Za-z0-9_.-]* 的裸键，且裸键不能被 YAML 读成非字符串：
// 含 "/" "#" 等（如 OpenAPI 路径键）加引号，避免被当成注释或标签；
// 数字开头加引号，避免响应码 "200" 这类键被解析成整数；
// true/false/null/~ 等保留词加引号，避免被解析成布尔或空值。
// 判据是「解析回来与 JSON 等值」——YAML 是 JSON 的超集，但裸标量有类型推断。
func isPlainKeySafe(k string) bool {
	if k == "" || isReservedKey(k) {
		return false
	}
	for i, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		case r == '_' || r == '-' || r == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// isReservedKey 判断键裸写时是否会被 YAML 读成布尔/空值（大小写不敏感）。
func isReservedKey(k string) bool {
	switch strings.ToLower(k) {
	case "true", "false", "null", "~", "yes", "no", "on", "off":
		return true
	}
	return false
}
