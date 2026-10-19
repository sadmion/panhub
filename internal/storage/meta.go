package storage

import "sync"

// Field 描述驱动的一个配置项。
type Field struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"` // text / password / bool / number
	Required    bool   `json:"required"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
}

// LoginField 是登录表单里的一个输入项。
type LoginField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"` // text / password
	Required bool   `json:"required"`
	Help     string `json:"help,omitempty"`
}

// Meta 是驱动的展示信息。
type Meta struct {
	Type   string  `json:"type"`
	Name   string  `json:"name"`
	Fields []Field `json:"fields"`
	Notes  string  `json:"notes,omitempty"`

	// Login 为 true 时前端显示「登录」面板。
	Login bool `json:"login"`
	// LoginModes 声明支持的登录方式，第一项为默认。
	LoginModes []string `json:"login_modes,omitempty"`
	// LoginFields 按登录方式声明需要的字段。
	LoginFields map[string][]LoginField `json:"login_fields,omitempty"`
}

var (
	metaMu sync.RWMutex
	metas  = map[string]Meta{}
)

// RegisterMeta 由驱动包的 init() 调用，声明配置字段。
func RegisterMeta(typ, name string, fields []Field) {
	metaMu.Lock()
	defer metaMu.Unlock()
	m := metas[typ]
	m.Type = typ
	m.Name = name
	m.Fields = fields
	metas[typ] = m
}

// RegisterLogin 声明登录方式与表单字段（RegisterMeta 之后调用）。
func RegisterLogin(typ string, modes []string, fields map[string][]LoginField) {
	metaMu.Lock()
	defer metaMu.Unlock()
	m := metas[typ]
	m.Type = typ
	m.Login = true
	m.LoginModes = modes
	m.LoginFields = fields
	metas[typ] = m
}

// MetaOf 返回某个驱动的元数据。
func MetaOf(typ string) (Meta, bool) {
	metaMu.RLock()
	defer metaMu.RUnlock()
	m, ok := metas[typ]
	return m, ok
}

// AllMeta 返回全部驱动元数据。
func AllMeta() []Meta {
	metaMu.RLock()
	defer metaMu.RUnlock()
	out := make([]Meta, 0, len(metas))
	for _, m := range metas {
		out = append(out, m)
	}
	return out
}
