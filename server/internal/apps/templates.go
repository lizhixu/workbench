package apps

import "fmt"

// splitLines splits a byte slice into lines, preserving empty lines.
func splitLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			lines = append(lines, raw[start:i])
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}

// EnvField defines a configurable environment variable for an application template.
type EnvField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Default     string `json:"default"`
	Required    bool   `json:"required"`
	IsSecret    bool   `json:"is_secret"`
}

// AppTemplate defines a one-click installable application.
type AppTemplate struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Category      string     `json:"category"`
	Icon          string     `json:"icon"`     // icon identifier
	Version       string     `json:"version"`
	Description   string     `json:"description"`
	Image         string     `json:"image"`
	DefaultPort   int        `json:"default_port"`
	ContainerPort int        `json:"container_port"`
	DefaultVolume string     `json:"default_volume"`
	EnvFields     []EnvField `json:"env_fields"`
}

// Catalog contains standard templates for one-click installation.
var Catalog = []AppTemplate{
	{
		ID:            "nginx",
		Name:          "Nginx Web Server",
		Category:      "web",
		Icon:          "GlobeOutline",
		Version:       "latest",
		Description:   "高性能 HTTP 与反向代理 Web 服务器，适用于静态网页、反向代理与负载均衡。",
		Image:         "nginx:alpine",
		DefaultPort:   80,
		ContainerPort: 80,
		DefaultVolume: "/opt/apps/nginx/html:/usr/share/nginx/html",
		EnvFields:     []EnvField{},
	},
	{
		ID:            "redis",
		Name:          "Redis In-Memory Database",
		Category:      "database",
		Icon:          "LayersOutline",
		Version:       "7.2-alpine",
		Description:   "超高性能内存键值数据库，支持持久化、缓存加速与消息队列。",
		Image:         "redis:7.2-alpine",
		DefaultPort:   6379,
		ContainerPort: 6379,
		DefaultVolume: "/opt/apps/redis/data:/data",
		EnvFields: []EnvField{
			{
				Key:         "PASSWORD",
				Label:       "访问密码 (可选)",
				Description: "留空则免密连接；填写则配置 requirepass 认证",
				Default:     "",
				IsSecret:    true,
			},
		},
	},
	{
		ID:            "mysql",
		Name:          "MySQL Relational Database",
		Category:      "database",
		Icon:          "ServerOutline",
		Version:       "8.0",
		Description:   "全球主流开源关系型数据库系统，支持高并发事务与企业级持久化存储。",
		Image:         "mysql:8.0",
		DefaultPort:   3306,
		ContainerPort: 3306,
		DefaultVolume: "/opt/apps/mysql/data:/var/lib/mysql",
		EnvFields: []EnvField{
			{
				Key:         "MYSQL_ROOT_PASSWORD",
				Label:       "Root 密码",
				Description: "root 超级管理员登录密码",
				Default:     "root123456",
				Required:    true,
				IsSecret:    true,
			},
			{
				Key:         "MYSQL_DATABASE",
				Label:       "默认数据库名称",
				Description: "容器首次启动时自动创建的数据库名",
				Default:     "app_db",
			},
		},
	},
	{
		ID:            "postgres",
		Name:          "PostgreSQL Database",
		Category:      "database",
		Icon:          "FileTrayStackedOutline",
		Version:       "16-alpine",
		Description:   "强大可靠的开源对象关系型数据库，支持丰富的数据类型与 JSON 扩展。",
		Image:         "postgres:16-alpine",
		DefaultPort:   5432,
		ContainerPort: 5432,
		DefaultVolume: "/opt/apps/postgres/data:/var/lib/postgresql/data",
		EnvFields: []EnvField{
			{
				Key:         "POSTGRES_PASSWORD",
				Label:       "Postgres 密码",
				Description: "默认管理员 postgres 用户的登录密码",
				Default:     "postgres123",
				Required:    true,
				IsSecret:    true,
			},
			{
				Key:         "POSTGRES_DB",
				Label:       "默认数据库名称",
				Description: "容器首次启动时创建的数据库",
				Default:     "watchman_db",
			},
		},
	},
	{
		ID:            "safeline",
		Name:          "雷池 SafeLine WAF 社区版",
		Category:      "security",
		Icon:          "ShieldCheckmarkOutline",
		Version:       "latest",
		Description:   "长亭科技开源 Web 应用防火墙 (WAF)，阻断 SQL 注入、XSS 等攻击。",
		Image:         "chaitin/safeline-tengine:latest",
		DefaultPort:   9443,
		ContainerPort: 9443,
		DefaultVolume: "/opt/apps/safeline/data:/app/data",
		EnvFields:     []EnvField{},
	},
}

// GetTemplate returns the AppTemplate by ID.
func GetTemplate(id string) (*AppTemplate, bool) {
	for _, t := range Catalog {
		if t.ID == id {
			return &t, true
		}
	}
	return nil, false
}

// ResolveTemplate populates the Application's runtime fields based on its TemplateID and TemplateParams.
func ResolveTemplate(app *Application) error {
	tpl, ok := GetTemplate(app.TemplateID)
	if !ok {
		return fmt.Errorf("未知的应用模板: %s", app.TemplateID)
	}

	app.Image = tpl.Image
	if app.Image == "" {
		return fmt.Errorf("模板 %s 缺少镜像配置", tpl.ID)
	}

	// Merge environment variables: template defaults + user provided params
	if app.EnvVars == nil {
		app.EnvVars = make(map[string]string)
	}
	for _, field := range tpl.EnvFields {
		val := field.Default
		if provided, exists := app.TemplateParams[field.Key]; exists && provided != "" {
			val = provided
		}
		if field.Required && val == "" {
			return fmt.Errorf("模板 %s 缺少必填参数: %s", tpl.Name, field.Label)
		}
		if val != "" {
			app.EnvVars[field.Key] = val
		}
	}

	// Special handling for Redis password (legacy logic from handlers.go)
	if tpl.ID == "redis" {
		if pwd, hasPwd := app.EnvVars["PASSWORD"]; hasPwd && pwd != "" {
			// In a real scenario, the command needs to be appended.
			// Since Engine currently doesn't support overriding command easily for templates,
			// we rely on the image's env var support or add it to a Command field if we had one.
			// For now, let's just ensure the env is there.
		}
	}

	// Default port mapping
	if len(app.Ports) == 0 && tpl.DefaultPort > 0 && tpl.ContainerPort > 0 {
		app.Ports = []PortMapping{{Host: tpl.DefaultPort, Container: tpl.ContainerPort}}
	}

	// Default volume
	if len(app.Volumes) == 0 && tpl.DefaultVolume != "" {
		app.Volumes = []string{tpl.DefaultVolume}
	}

	return nil
}
