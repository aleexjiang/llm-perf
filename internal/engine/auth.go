// auth.go：认证格式抽象。客户环境的 key 形态五花八门——标准 `Authorization: Bearer <key>`、
// 网关裸 key（`Authorization: <key>` 无 Bearer 前缀）、自定义 header（如 X-API-Key）。
// 之前 Bearer 硬编码在 probe 与 client 共 3 处，裸 key 环境直接 401。
package engine

import (
	"net/http"
)

// Auth 认证方案（对应 config 的 auth_scheme / auth_header）。
//   - Scheme "bearer"（默认，含空值）：`<Header>: Bearer <key>`，Header 默认 Authorization
//   - Scheme "raw"：`<Header>: <key>`（裸 key，不加 Bearer 前缀）
//   - Scheme "none"：即使有 key 也不带认证头（免认证端点）
type Auth struct {
	Scheme string
	Header string // 空 = Authorization
}

// Normalize 兜底默认值（空 Scheme = bearer）。
func (a Auth) Normalize() Auth {
	if a.Scheme == "" {
		a.Scheme = "bearer"
	}
	if a.Header == "" {
		a.Header = "Authorization"
	}
	return a
}

// Apply 把认证头写进请求。key 为空或 scheme=none 时不写。
func (a Auth) Apply(req *http.Request, key string) {
	a = a.Normalize()
	if a.Scheme == "none" || key == "" {
		return
	}
	v := key
	if a.Scheme != "raw" {
		v = "Bearer " + key
	}
	req.Header.Set(a.Header, v)
}

// Describe 返回人类可读描述（probe 日志用，不泄露 key 本体）。
func (a Auth) Describe() string {
	a = a.Normalize()
	switch a.Scheme {
	case "none":
		return "无认证（auth_scheme=none）"
	case "raw":
		return "裸 key（" + a.Header + ": <key>，无 Bearer 前缀）"
	default:
		return "Bearer（" + a.Header + ": Bearer <key>）"
	}
}
