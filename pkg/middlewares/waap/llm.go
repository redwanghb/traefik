package waap

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redwanghb/coraza/v3/debuglog"
	"github.com/tidwall/gjson"
)

// 配置相关变量
// TODO 增加根据配置或者自动识别生成LLMRequestPath和LLMResponsePath功能，修改API Address
var (
	APIAddress      = "http://localhost:8000"
	LLMRquestPath   = map[string]string{"ollama": "message.0.content"}
	LLMResponsePath = map[string]string{"ollama": "message.content"}
	//配置各大模型API框架支持的content-type
	ContentTypes = []string{
		"application/json",
		"application/x-ndjson",
		"application/x-www-form-urlencoded",
	}
	MaxIdleConns        = 10000
	MaxIdleConnsPerHost = 10000
	LlmGuardClient      *LlmGuard
)

func ContainsContentType(contentType string) bool {
	contentType = strings.ToLower(contentType)
	for _, ct := range ContentTypes {
		if ct == contentType {
			return true
		}
	}
	return false
}

// 定义接口调用客户端池
type LlmGuard struct {
	clientPool sync.Pool
	debuglog   debuglog.Logger
	config     *Config
}

// Obsolete
// 废弃此方法不再使用
func (l *LlmGuard) loadConfig(path string) {
	//判定配置文件是否存在
	if !FileExists(path) {
		l.debuglog.Error().Str("llm config path %s is not exists", path)
	}
	//读取配置文件
	configData, err := os.ReadFile(path)
	if err != nil {
		l.debuglog.Error().Err(err).Msg("failed to convert llm config to byte data")
	}

	//将配置文件转换成结构体并存储到config成员
	err = json.Unmarshal(configData, l.config)
	if err != nil {
		l.debuglog.Error().Err(err).Msg("failed to convert llm config to struct data")
	}
}

func newClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        MaxIdleConns,
			MaxIdleConnsPerHost: MaxIdleConnsPerHost,
			IdleConnTimeout:     30 * time.Second,
			DisableKeepAlives:   false,
		},
	}
}

func initLlmGuardClient() {
	LlmGuardClient = &LlmGuard{
		clientPool: sync.Pool{
			New: func() any {
				return newClient()
			},
		},
		debuglog: debuglog.Noop(),
		config:   &Config{},
	}
}

func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type Config struct {
	Address  string    `json:"address"`
	LLMPaths []LLMPath `json:"llmpaths"`
}

type LLMPath struct {
	Request  string `json:"request"`
	Response string `json:"response"`
}

// 记录全部LLM框架接口的问题和答案的json路径
var RequestPaths []string
var ResponsePaths []string

// 将配置文件中的Request和Response添加到json路径列表中
func jsonPath() {
	for _, reqPath := range LLMRquestPath {
		RequestPaths = append(RequestPaths, reqPath)
	}

	for _, resPath := range LLMResponsePath {
		ResponsePaths = append(ResponsePaths, resPath)
	}
}

type DATATYPE int

const (
	REQUESTBODY DATATYPE = iota
	RESPONSEBODY
)

// 从给定的字符串中提取请求或者应答数据，如果提取不到，返回""和false， 如果提取到内容返回对应的内容和true
func ContentExtractFromJSONDATA(data string, datatype DATATYPE) (string, bool) {
	switch datatype {
	case REQUESTBODY:
		return RequestBodyExtract(data)
	default:
		return ResponseBodyExtract(data)
	}
}

func RequestBodyExtract(data string) (string, bool) {
	for _, jsonPath := range RequestPaths {
		result := gjson.Get(data, jsonPath)
		if result.Exists() {
			return result.String(), true
		}
	}
	return "", false
}

// 输入json结构体字符串，然后从中提取符合支持的大模型回答json结构体里的答案
func ResponseBodyExtract(data string) (string, bool) {
	for _, jsonPath := range ResponsePaths {
		result := gjson.Get(data, jsonPath)
		if result.Exists() {
			return result.String(), true
		}
	}
	return "", false
}

// TODO 初始化相关的接口和Client
func init() {
	// 初始化客户端
	initLlmGuardClient()

	// 设置API地址
	LlmGuardClient.config.Address = APIAddress

	// 加载已知大模型API中内容的路径信息
	jsonPath()
}
