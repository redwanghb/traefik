package server

import (
	"context"

	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/config/runtime"
	"github.com/traefik/traefik/v3/pkg/config/static"
	"github.com/traefik/traefik/v3/pkg/server/middleware"
	tcpmiddleware "github.com/traefik/traefik/v3/pkg/server/middleware/tcp"
	"github.com/traefik/traefik/v3/pkg/server/router"
	tcprouter "github.com/traefik/traefik/v3/pkg/server/router/tcp"
	udprouter "github.com/traefik/traefik/v3/pkg/server/router/udp"
	"github.com/traefik/traefik/v3/pkg/server/service"
	tcpsvc "github.com/traefik/traefik/v3/pkg/server/service/tcp"
	udpsvc "github.com/traefik/traefik/v3/pkg/server/service/udp"
	"github.com/traefik/traefik/v3/pkg/tcp"
	"github.com/traefik/traefik/v3/pkg/tls"
	"github.com/traefik/traefik/v3/pkg/udp"
)

// RouterFactory the factory of TCP/UDP routers.
type RouterFactory struct {
	entryPointsTCP  []string
	entryPointsUDP  []string
	allowACMEByPass map[string]bool

	managerFactory *service.ManagerFactory

	pluginBuilder middleware.PluginsBuilder

	observabilityMgr *middleware.ObservabilityMgr
	tlsManager       *tls.Manager

	dialerManager *tcp.DialerManager

	cancelPrevState func()
}

// NewRouterFactory creates a new RouterFactory.
func NewRouterFactory(staticConfiguration static.Configuration, managerFactory *service.ManagerFactory, tlsManager *tls.Manager,
	observabilityMgr *middleware.ObservabilityMgr, pluginBuilder middleware.PluginsBuilder, dialerManager *tcp.DialerManager,
) *RouterFactory {
	//判断静态配置中CertificatesResolvers中是否有TLSChallenge配置
	handlesTLSChallenge := false
	for _, resolver := range staticConfiguration.CertificatesResolvers {
		if resolver.ACME != nil && resolver.ACME.TLSChallenge != nil {
			handlesTLSChallenge = true
			break
		}
	}

	//存储每一个entrypoint的AllowACMEByPass配置
	//如果没有TLSChallenge配置，或者entrypoint中设置了AllowACMEByPass配置，则所有entrypoint都允许ACMEByPass
	//根据EntryPoint的协议类型，将entrypoint加入到entryPointsTCP或entryPointsUDP中
	//allowACMEByPass Map存储到RouterFactory中
	allowACMEByPass := map[string]bool{}
	var entryPointsTCP, entryPointsUDP []string
	for name, ep := range staticConfiguration.EntryPoints {
		allowACMEByPass[name] = ep.AllowACMEByPass || !handlesTLSChallenge

		protocol, err := ep.GetProtocol()
		if err != nil {
			// Should never happen because Traefik should not start if protocol is invalid.
			log.Error().Err(err).Msg("Invalid protocol")
		}

		if protocol == "udp" {
			entryPointsUDP = append(entryPointsUDP, name)
		} else {
			entryPointsTCP = append(entryPointsTCP, name)
		}
	}

	return &RouterFactory{
		entryPointsTCP:   entryPointsTCP,
		entryPointsUDP:   entryPointsUDP,
		managerFactory:   managerFactory,
		observabilityMgr: observabilityMgr,
		tlsManager:       tlsManager,
		pluginBuilder:    pluginBuilder,
		dialerManager:    dialerManager,
		allowACMEByPass:  allowACMEByPass,
	}
}

// CreateRouters creates new TCPRouters and UDPRouters.
// runtime.Configuration保留了当前实例的配置
func (f *RouterFactory) CreateRouters(rtConf *runtime.Configuration) (map[string]*tcprouter.Router, map[string]udp.Handler) {
	if f.cancelPrevState != nil {
		f.cancelPrevState()
	}

	// TODO 将RouterFactory的cancelPrevState设置为ctx.Cancel，这样在下次调用CreateRouters时可以取消上一个状态，待确认在哪里接收ctx.Done()
	var ctx context.Context
	ctx, f.cancelPrevState = context.WithCancel(context.Background())

	// HTTP
	// 创建service.Manager，存储了runtime.Configuration.Services, RouterFactory.observabilityMgr, RouterFactory.routinesPool, RouterFactory.transportManager,
	// RouterFactory.proxyBuilderapiHandler, restHandler, metricsHandler, pingHandler, dashboardHandler, acmeHTTPHandler
	// 这里只存储了HTTP的Services信息
	serviceManager := f.managerFactory.Build(rtConf)

	// 创建middleware.Builder对象，存储了runtime.Configuration.Middlewares, serviceManager, RouterFactory.pluginBuilder
	middlewaresBuilder := middleware.NewBuilder(rtConf.Middlewares, serviceManager, f.pluginBuilder)

	// 创建router.Manager对象，存储了runtime.Configuration, serviceManager, middlewaresBuilder, RouterFactory.observabilityMgr, RouterFactory.tlsManager
	routerManager := router.NewManager(rtConf, serviceManager, middlewaresBuilder, f.observabilityMgr, f.tlsManager)

	// 创建handlersNonTLS和handlersTLS对象，返回的为一个map[entrypoint名称]http.Handler，这里返回的map中，每一个http.Handler都是最终返回NOTFound的http.Handler
	// 这里的handlerNonTLS最终会存储到TCPEntryPoint.httpServer.Switcher中，同时也会存储到TCPEntryPoint.httpServer.Server.Handler中，在TCPEntryPoint.Start()
	// 中被最终调用处理，如果是一个HTTP TCPEntryPoint，最终是使用handlersNonTLS来进行处理的
	handlersNonTLS := routerManager.BuildHandlers(ctx, f.entryPointsTCP, false)
	handlersTLS := routerManager.BuildHandlers(ctx, f.entryPointsTCP, true)

	//执行HealthCheck
	serviceManager.LaunchHealthCheck(ctx)

	// TCP
	// 创建一个TCP.Manager对象，可以基于ServiceInfo创建TCP.Handler，提供tcp.Handler方法
	// dialerManager给代理使用
	// runtime.Configuration提取里面的TCPServiceInfo信息，用于构建tcp.Handler
	svcTCPManager := tcpsvc.NewManager(rtConf, f.dialerManager)

	// 基于TCPMiddlerwares构建tcpmiddleware.Builder，可以用于构建tcp.Chain(TCP中间件集合)
	middlewaresTCPBuilder := tcpmiddleware.NewBuilder(rtConf.TCPMiddlewares)

	// 构建tcp.Manager，使用tcp.Manager构建tcp.Router
	// 需要用到TCPServiceInfo、tcpmiddleware.Builder、httpHandler和httpsHandler
	// 调用BuildHandlers创建相同的tcp.Router，然后替换到当前系统运行的路由信息中
	rtTCPManager := tcprouter.NewManager(rtConf, svcTCPManager, middlewaresTCPBuilder, handlersNonTLS, handlersTLS, f.tlsManager)
	routersTCP := rtTCPManager.BuildHandlers(ctx, f.entryPointsTCP)

	for ep, r := range routersTCP {
		if allowACMEByPass, ok := f.allowACMEByPass[ep]; ok && allowACMEByPass {
			r.EnableACMETLSPassthrough()
		}
	}

	// UDP
	svcUDPManager := udpsvc.NewManager(rtConf)
	rtUDPManager := udprouter.NewManager(rtConf, svcUDPManager)
	routersUDP := rtUDPManager.BuildHandlers(ctx, f.entryPointsUDP)

	rtConf.PopulateUsedBy()

	return routersTCP, routersUDP
}
