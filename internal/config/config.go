// Package config 从环境变量和 .env 文件读取配置。
package config

import (
	"bufio"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 是整个应用的运行配置。
type Config struct {
	Env         string
	Port        string
	SiteName    string
	SiteURL     string
	AppSecret   string
	SessionTTL  time.Duration
	DatabaseURL string
	// DBSchema 非空时，所有表都建在这个 schema 里（而不是 public）。演示数据用它和正式数据隔开。
	DBSchema   string
	DBMaxConns int

	// PoWBits 是登录、注册人机校验的难度（1–28），默认 20。
	PoWBits int

	// SMTP 发信。SMTPHost 为空时：开发环境把邮件写进日志，生产环境不能注册和找回密码。
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	// MailFrom 是发件人，如 "影牛 <noreply@example.com>"。
	MailFrom string
	// MailHourlyLimit 是全站每小时最多发几封，防止被刷。
	MailHourlyLimit int
	// ContactEmail 是对外公布的联系邮箱（版权投诉等）。不配置时用发件人地址。
	ContactEmail string

	// Telegram 图床：配置了机器人和频道时，代理过的图片会备份到频道，之后优先从频道取。
	TelegramToken  string
	TelegramChatID string
	TelegramAPI    string

	// DoubanProxy 是访问豆瓣接口用的出站代理（http://、https:// 或 socks5://），留空时直连。
	DoubanProxy string
	// HongguoSync 为 true 时后台定期从红果短剧官网抓榜单、重点剧目、分类和新上架的短剧。
	// 关掉以后，已有的短剧仍会在有人打开、资料还没抓到时去抓一次。
	HongguoSync bool

	JobsInWeb         bool
	WorkerConcurrency int

	TMDBToken         string
	WikidataEndpoint  string
	WikidataUserAgent string
	// AI 写语义描述，用 OpenAI 格式的 /chat/completions 接口
	AIBaseURL string
	AIKey     string
	AIModel   string
	// 向量模型，用 OpenAI 格式的 /embeddings 接口（Ollama 的地址是 http://主机:11434/v1）
	EmbeddingBaseURL string
	EmbeddingKey     string
	EmbeddingModel   string
	DanmuBase        string
	// 只信任这些地址发来的 X-Forwarded-For / X-Forwarded-Proto（反向代理所在的地址或网段）
	TrustedProxies []string
	// 临时：示例订阅文件的路径（cmd/migrate-legacy -sources 导出），网站在 /sub/example.json 提供。为空表示关闭
	ExampleSubscription string
}

// Dev 表示是否是开发环境。
func (c Config) Dev() bool { return c.Env != "production" }

// Load 读取配置。已经存在的环境变量优先于 .env 文件。
func Load() (Config, error) {
	LoadDotEnv()
	cfg := Config{
		Env:               get("APP_ENV", "development"),
		Port:              get("PORT", "5200"),
		SiteName:          get("SITE_NAME", "Moovie 影牛"),
		SiteURL:           strings.TrimRight(get("SITE_URL", "http://localhost:5200"), "/"),
		AppSecret:         get("APP_SECRET", ""),
		SessionTTL:        time.Duration(getInt("SESSION_HOURS", 720)) * time.Hour,
		DatabaseURL:       DatabaseURL(),
		DBSchema:          get("DB_SCHEMA", ""),
		DBMaxConns:        getInt("DB_MAX_CONNS", 10),
		PoWBits:           getInt("POW_BITS", 20),
		SMTPHost:          get("SMTP_HOST", ""),
		SMTPPort:          getInt("SMTP_PORT", 465),
		SMTPUsername:      get("SMTP_USER", ""),
		SMTPPassword:      get("SMTP_PASS", ""),
		MailFrom:          get("SMTP_FROM", ""),
		ContactEmail:      get("CONTACT_EMAIL", ""),
		TelegramToken:     get("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:    get("TELEGRAM_CHAT_ID", ""),
		TelegramAPI:       get("TELEGRAM_API_BASE", ""),
		DoubanProxy:       get("DOUBAN_PROXY", ""),
		HongguoSync:       getBool("HONGGUO_SYNC", true),
		MailHourlyLimit:   getInt("MAIL_HOURLY_LIMIT", 200),
		JobsInWeb:         getBool("JOBS_IN_WEB", true),
		WorkerConcurrency: getInt("WORKER_CONCURRENCY", 2),
		TMDBToken:         get("TMDB_API_TOKEN", ""),
		WikidataEndpoint:  get("WIKIDATA_SPARQL_URL", "https://query.wikidata.org/sparql"),
		WikidataUserAgent: get("WIKIDATA_USER_AGENT", "MoovieBot/2.0"),
		AIBaseURL:         strings.TrimRight(get("AI_BASE_URL", ""), "/"),
		AIKey:             get("AI_API_KEY", ""),
		AIModel:           get("AI_MODEL", ""),
		EmbeddingBaseURL:  strings.TrimRight(get("EMBEDDING_BASE_URL", ""), "/"),
		EmbeddingKey:      get("EMBEDDING_API_KEY", ""),
		// 迁移过来的向量是这个模型算的。换模型就得把所有作品的向量重算一遍，否则新旧向量不能放在一起比较。
		EmbeddingModel:      get("EMBEDDING_MODEL", "quentinz/bge-base-zh-v1.5"),
		DanmuBase:           strings.TrimRight(get("DANMU_API_BASE", ""), "/"),
		ExampleSubscription: get("EXAMPLE_SUBSCRIPTION_FILE", ""),
		// 默认只信任本机和内网：反向代理一般就在这些地址；外面直接连进来的请求，伪造的转发头不算数。
		TrustedProxies: strings.Split(get("TRUSTED_PROXIES", "127.0.0.1,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"), ","),
	}
	if cfg.ContactEmail == "" {
		if parsed, err := mail.ParseAddress(cfg.MailFrom); err == nil {
			cfg.ContactEmail = parsed.Address
		}
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL 未设置")
	}
	if cfg.AppSecret == "" && cfg.Dev() {
		// 开发环境允许不配：用固定值，重启后登录状态还在。生产环境必须配置。
		cfg.AppSecret = "moovie-development-secret"
	}
	if cfg.AppSecret == "" || (!cfg.Dev() && cfg.AppSecret == "change-me") {
		return cfg, fmt.Errorf("APP_SECRET 未设置或仍是默认值")
	}
	return cfg, nil
}

// LoadDotEnv 从当前目录向上找 .env 并加载，找到第一个为止。
// 向上找是为了让 go test 在各个包目录里运行时也能读到项目根目录的 .env。
func LoadDotEnv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for range 6 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			loadFile(filepath.Join(dir, ".env"))
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func loadFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}
}

// DatabaseURL 返回数据库地址。优先用 DATABASE_URL；没有时用 DB_HOST、DB_PORT、DB_USER、
// DB_PASSWORD、DB_NAME、DB_SSLMODE、DB_TIMEZONE 这组变量拼出来。都没有时返回空字符串。
func DatabaseURL() string {
	if value := get("DATABASE_URL", ""); value != "" {
		return value
	}
	host, name := get("DB_HOST", ""), get("DB_NAME", "")
	if host == "" || name == "" {
		return ""
	}
	address := url.URL{
		Scheme: "postgres", Host: host + ":" + get("DB_PORT", "5432"), Path: "/" + name,
		User: url.UserPassword(get("DB_USER", "postgres"), get("DB_PASSWORD", "")),
	}
	query := url.Values{"sslmode": {get("DB_SSLMODE", "disable")}}
	if zone := get("DB_TIMEZONE", ""); zone != "" {
		query.Set("timezone", zone)
	}
	address.RawQuery = query.Encode()
	return address.String()
}

func get(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func getInt(key string, fallback int) int {
	if value, err := strconv.Atoi(get(key, "")); err == nil {
		return value
	}
	return fallback
}

func getBool(key string, fallback bool) bool {
	if value, err := strconv.ParseBool(get(key, "")); err == nil {
		return value
	}
	return fallback
}
