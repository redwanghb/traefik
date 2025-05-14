package waap

import (
	"context"
	"fmt"
	"net/http"

	"github.com/redwanghb/coraza/v3"
	"github.com/redwanghb/coraza/v3/debuglog"
	txhttp "github.com/redwanghb/coraza/v3/http"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/config/dynamic"
	"github.com/traefik/traefik/v3/pkg/middlewares"
)

const (
	typeName = "WAAP"
)

var DefaultContentTypes = []string{
	"text/plain",
	"text/xml",
	"text/html",
	"application/json",
	"application/xml",
}

// Web Application and API Protection Middleware
type waap struct {
	handler http.Handler
}

// LLM内容检测API接口配置
type LlmAPI interface {
	QuestionDetect(reqBody string) *Result
	AnswerDetect() *Result
}

// 检测结果
type Result struct {
	// 返回内容标签
	Labels []Label
	// 检测结果
	Status bool
	// 详细信息
	Message any
}

type Label string

// 创建Handler对象
func NewWaap(ctx context.Context, next http.Handler, conf dynamic.WAAP, name string) (http.Handler, error) {
	logger := middlewares.GetLogger(ctx, name, typeName)
	logger.Debug().Msgf("Creating middleware %s", typeName)
	waf, err := newWaf(conf, logger)
	if err != nil {
		return nil, err
	}

	handler := txhttp.WrapHandler(waf, next)
	return &waap{
		handler: handler,
	}, nil
}

func (w *waap) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	w.handler.ServeHTTP(rw, req)
}

// 创建waf对象
func newWaf(conf dynamic.WAAP, logger *zerolog.Logger) (coraza.WAF, error) {
	// 检查ResponseMIME配置，没有的话使用默认配置
	var mimes []string
	if conf.ResponseMIMEs == nil {
		mimes = DefaultContentTypes
	} else {
		mimes = conf.ResponseMIMEs
	}

	// 检查responseBody Size配置
	responseBodySize := conf.ResponseSize
	if responseBodySize < 4096 {
		responseBodySize = 4096
	}

	// 创建waf config
	wafConfig := coraza.NewWAFConfig().WithRequestBodyAccess().
		WithResponseBodyAccess().
		WithResponseBodyLimit(responseBodySize).
		WithResponseBodyMimeTypes(mimes).
		WithDebugLogger(debuglog.Noop().WithOutput(logger).WithLevel(debuglog.Level(log.Logger.GetLevel())))

	if conf.RuleDir == "" && conf.RulePath != "" {
		wafConfig.WithDirectivesFromFile(conf.RulePath)
	} else if conf.RuleDir != "" && conf.RulePath == "" {
		wafConfig.WithDirectives(conf.RuleDir)
	} else {
		return nil, fmt.Errorf("Only one option in the rule path and rule file needs to be configured")
	}

	waf, err := coraza.NewWAF(wafConfig)
	if err != nil {
		return nil, err
	}
	return waf, nil
}
