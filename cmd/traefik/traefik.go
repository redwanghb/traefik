package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	stdlog "log"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/go-acme/lego/v4/challenge"
	gokitmetrics "github.com/go-kit/kit/metrics"
	"github.com/rs/zerolog/log"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"github.com/traefik/paerser/cli"
	"github.com/traefik/traefik/v3/cmd"
	"github.com/traefik/traefik/v3/cmd/healthcheck"
	cmdVersion "github.com/traefik/traefik/v3/cmd/version"
	tcli "github.com/traefik/traefik/v3/pkg/cli"
	"github.com/traefik/traefik/v3/pkg/collector"
	"github.com/traefik/traefik/v3/pkg/config/dynamic"
	"github.com/traefik/traefik/v3/pkg/config/runtime"
	"github.com/traefik/traefik/v3/pkg/config/static"
	"github.com/traefik/traefik/v3/pkg/logs"
	"github.com/traefik/traefik/v3/pkg/metrics"
	"github.com/traefik/traefik/v3/pkg/middlewares/accesslog"
	"github.com/traefik/traefik/v3/pkg/provider/acme"
	"github.com/traefik/traefik/v3/pkg/provider/aggregator"
	"github.com/traefik/traefik/v3/pkg/provider/tailscale"
	"github.com/traefik/traefik/v3/pkg/provider/traefik"
	"github.com/traefik/traefik/v3/pkg/proxy"
	"github.com/traefik/traefik/v3/pkg/proxy/httputil"
	"github.com/traefik/traefik/v3/pkg/safe"
	"github.com/traefik/traefik/v3/pkg/server"
	"github.com/traefik/traefik/v3/pkg/server/middleware"
	"github.com/traefik/traefik/v3/pkg/server/service"
	"github.com/traefik/traefik/v3/pkg/tcp"
	traefiktls "github.com/traefik/traefik/v3/pkg/tls"
	"github.com/traefik/traefik/v3/pkg/tracing"
	"github.com/traefik/traefik/v3/pkg/types"
	"github.com/traefik/traefik/v3/pkg/version"
)

func main() {
	// traefik config inits
	tConfig := cmd.NewTraefikConfiguration()

	// 后续加载配置使用，cli.Execute中会调用run函数，run函数会逐个调用loaders中的Load方法
	// DeprecationLoader主要用于配置兼容性检查，检查FileLoader、FlagLoader、EnvLoader中是否配置了在当前版本已经废弃掉的配置项
	// FileLoader对应的是--configFile参数，用于加载静态配置
	// Flagloader对应的是命令行中的参数，用于加载静态配置
	// EnvLoader用于检查环境变量中是否包含traefik_对应的环境变量配置
	// 加载顺序是file > flag > env
	loaders := []cli.ResourceLoader{&tcli.DeprecationLoader{}, &tcli.FileLoader{}, &tcli.FlagLoader{}, &tcli.EnvLoader{}}

	cmdTraefik := &cli.Command{
		Name: "traefik",
		Description: `Traefik is a modern HTTP reverse proxy and load balancer made to deploy microservices with ease.
Complete documentation is available at https://traefik.io`,
		Configuration: tConfig,
		Resources:     loaders,
		Run: func(_ []string) error {
			return runCmd(&tConfig.Configuration)
		}, //最后会运行此函数
	}

	// 将这些Command加入到subCommand中
	// 定义一个healthcheck子命令，用于实时检查当前系统的健康状态
	err := cmdTraefik.AddCommand(healthcheck.NewCmd(&tConfig.Configuration, loaders))
	if err != nil {
		stdlog.Println(err)
		os.Exit(1)
	}

	// 定义一个子命令version，用于打印应用构建的版本号、操作系统和架构等信息
	err = cmdTraefik.AddCommand(cmdVersion.NewCmd())
	if err != nil {
		stdlog.Println(err)
		os.Exit(1)
	}
	// 主要运行函数
	err = cli.Execute(cmdTraefik)
	if err != nil {
		log.Error().Err(err).Msg("Command error")
		logrus.Exit(1)
	}

	logrus.Exit(0)
}

func runCmd(staticConfiguration *static.Configuration) error {
	// 设置日志系统，同时将标准库log和logrus的日志输出到zerolog中，统一logger
	if err := setupLogger(staticConfiguration); err != nil {
		return fmt.Errorf("setting up logger: %w", err)
	}

	// 配置HTTP默认传输代理
	http.DefaultTransport.(*http.Transport).Proxy = http.ProxyFromEnvironment

	// 根据静态配置设置默认值，将配置中不存在的配置补全为默认值
	staticConfiguration.SetEffectiveConfiguration()

	// 检查静态配置，主要检查以下配置
	// CertificatesResolvers
	// Accesslog
	// Log
	// Metrics
	// Tracing
	// API
	if err := staticConfiguration.ValidateConfiguration(); err != nil {
		return err
	}

	log.Info().Str("version", version.Version).
		Msgf("Traefik version %s built on %s", version.Version, version.BuildDate)

	// 这里是为了debug打印静态配置的内容
	jsonConf, err := json.Marshal(staticConfiguration)
	if err != nil {
		log.Error().Err(err).Msg("Could not marshal static configuration")
		log.Debug().Interface("staticConfiguration", staticConfiguration).Msg("Static configuration loaded [struct]")
	} else {
		log.Debug().RawJSON("staticConfiguration", jsonConf).Msg("Static configuration loaded [json]")
	}

	// TODO(wanghongbo) 后续研究如何处理，根据静态配置确定是否检查是否存在新的版本，后续最好关闭或者改写为自己发布的版本路径
	if staticConfiguration.Global.CheckNewVersion {
		checkNewVersion()
	}

	// TODO(wanghongbo) 收集用户配置信息，将用户配置信息发送到traefik的服务器，最好关闭，或者将收集的信息发送到自己的服务器，需要通知用户开启或关闭，可以在配置中关闭，将来支持集中管理的时候再重写
	stats(staticConfiguration)

	// TODO(wanghongbo) 创建服务器，其中一部分与Entrypoint无关的内容未理解，例如observabilityMgr、pluginBuilder、managerFactory等
	svr, err := setupServer(staticConfiguration)
	if err != nil {
		return err
	}

	// 创建用于接收系统kill信号或者ctrl c信号的上下文
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	// TODO(wanghongbo) 待确认，如何运行起来的
	if staticConfiguration.Ping != nil {
		staticConfiguration.Ping.WithContext(ctx)
	}

	// READING(wanghongbo) 启动服务器，需要深度理解里面执行内容的作用，需要搞清楚watchconfigure的作用
	svr.Start(ctx)
	defer svr.Close()

	// 通知Systemd服务已经启动完成
	// 这里的daemon.SdNotify是systemd的一个函数，用于通知systemd服务的状态
	// 这里的READY=1表示服务已经准备好，可以接收请求
	// 可以通过systemd监控和限制服务的资源占用情况和服务是否正常运行
	sent, err := daemon.SdNotify(false, "READY=1")
	if !sent && err != nil {
		log.Error().Err(err).Msg("Failed to notify")
	}

	// 实现systemd watchdog功能；
	// watchdog是systemd的一个功能，用于监控服务的健康状态
	// 如果服务在指定时间内没有发送心跳，systemd会认为服务已经崩溃并重启它
	t, err := daemon.SdWatchdogEnabled(false)
	if err != nil {
		log.Error().Err(err).Msg("Could not enable Watchdog")
	} else if t != 0 {
		// Send a ping each half time given
		t /= 2
		log.Info().Msgf("Watchdog activated with timer duration %s", t)
		safe.Go(func() {
			tick := time.Tick(t)
			for range tick {
				//TODO  发送心跳，通过http head方法向entry的“/ping”路径发送请求，这里需要确认HTTP请求返回的error代表什么含义
				resp, errHealthCheck := healthcheck.Do(*staticConfiguration)
				if resp != nil {
					_ = resp.Body.Close()
				}

				if staticConfiguration.Ping == nil || errHealthCheck == nil {
					if ok, _ := daemon.SdNotify(false, "WATCHDOG=1"); !ok {
						log.Error().Msg("Fail to tick watchdog")
					}
				} else {
					log.Error().Err(errHealthCheck).Send()
				}
			}
		})
	}

	svr.Wait()
	log.Info().Msg("Shutting down")
	return nil
}

func setupServer(staticConfiguration *static.Configuration) (*server.Server, error) {
	// 将静态配置中的providers转换为providerAggregator对象，在过程中会调用Provider的Init方法
	providerAggregator := aggregator.NewProviderAggregator(*staticConfiguration.Providers)

	ctx := context.Background()
	routinesPool := safe.NewPool(ctx)

	// adds internal provider
	// 把静态配置添加为internal provider
	err := providerAggregator.AddProvider(traefik.New(*staticConfiguration))
	if err != nil {
		return nil, err
	}

	// ACME
	// ACME(Automatic Certificate Management Environment)是一个自动化证书管理协议
	// 它允许用户自动申请、续期和撤销SSL/TLS证书
	// tlsmanager是一个tls配置管理器，包含了TLS证书的存储、管理、TLS协议配置等信息
	tlsManager := traefiktls.NewManager()
	// 创建一个ACME http-01 challenger.Provider对象，用于处理ACME协议的挑战，用来验证token、domain、keyauth
	httpChallengeProvider := acme.NewChallengeHTTP()

	// 创建tls-alpn-01挑战对象，用于处理证书的挑战，只不过额外实现了Provider接口
	// 然后将此provider加入到providerAggregator中的providers成员中
	tlsChallengeProvider := acme.NewChallengeTLSALPN()
	err = providerAggregator.AddProvider(tlsChallengeProvider)
	if err != nil {
		return nil, err
	}

	// 创建acme.Provider对象集合，acmeProvider实现了Provider接口的对象，将其加入到providerAggregator中
	acmeProviders := initACMEProvider(staticConfiguration, providerAggregator, tlsManager, httpChallengeProvider, tlsChallengeProvider, routinesPool)

	// Tailscale
	// 创建tailscale.Provider对象集合，tailscaleProvider实现了Provider接口的对象，将其加入到providerAggregator中
	// 使用Tailscale获取证书，自动更新证书
	tsProviders := initTailscaleProviders(staticConfiguration, providerAggregator)

	// Observability
	// 配置metric用于监控统计信息、Accesslog用于记录每一次请求的日志、Tracing用于分布式追踪，Metric信息根据配置写入OLTP、StatSD、Prometheus等
	metricRegistries := registerMetricClients(staticConfiguration.Metrics)
	var semConvMetricRegistry *metrics.SemConvMetricsRegistry
	if staticConfiguration.Metrics != nil && staticConfiguration.Metrics.OTLP != nil {
		semConvMetricRegistry, err = metrics.NewSemConvMetricRegistry(ctx, staticConfiguration.Metrics.OTLP)
		if err != nil {
			return nil, fmt.Errorf("unable to create SemConv metric registry: %w", err)
		}
	}
	metricsRegistry := metrics.NewMultiRegistry(metricRegistries)
	accessLog := setupAccessLog(staticConfiguration.AccessLog)
	tracer, tracerCloser := setupTracing(staticConfiguration.Tracing)
	observabilityMgr := middleware.NewObservabilityMgr(*staticConfiguration, metricsRegistry, semConvMetricRegistry, accessLog, tracer, tracerCloser)

	// Entrypoints
	// 创建TCP的入口点
	serverEntryPointsTCP, err := server.NewTCPEntryPoints(staticConfiguration.EntryPoints, staticConfiguration.HostResolver, metricsRegistry)
	if err != nil {
		return nil, err
	}
	// 创建UDP的入口点
	serverEntryPointsUDP, err := server.NewUDPEntryPoints(staticConfiguration.EntryPoints)
	if err != nil {
		return nil, err
	}

	// 根据静态配置的API选项设置version中的DisableDashboardAd，具体作用未知
	if staticConfiguration.API != nil {
		version.DisableDashboardAd = staticConfiguration.API.DisableDashboardAd
	}

	// Plugins
	// Reading
	// 创建plugin Logger
	pluginLogger := log.Ctx(ctx).With().Logger()
	// 判断是否启用了插件，主要判断静态配置中的Experimental是否存在Plugins和LocalPlugins
	// 如果启用了插件，则将Plugins和LocalPlugins的名称添加到pluginList中
	hasPlugins := staticConfiguration.Experimental != nil && (staticConfiguration.Experimental.Plugins != nil || staticConfiguration.Experimental.LocalPlugins != nil)
	if hasPlugins {
		pluginsList := slices.Collect(maps.Keys(staticConfiguration.Experimental.Plugins))
		pluginsList = append(pluginsList, slices.Collect(maps.Keys(staticConfiguration.Experimental.LocalPlugins))...)

		pluginLogger = pluginLogger.With().Strs("plugins", pluginsList).Logger()
		pluginLogger.Info().Msg("Loading plugins...")
	}

	// 根据Experimental配置的插件构建Builder，里面包含了middleware类型和provider类型的插件
	// middleware的插件可以是wasm或yaegi两种类型
	// provider插件只支持yaegi类型
	pluginBuilder, err := createPluginBuilder(staticConfiguration)
	if err != nil && staticConfiguration.Experimental != nil && staticConfiguration.Experimental.AbortOnPluginFailure {
		return nil, fmt.Errorf("plugin: failed to create plugin builder: %w", err)
	}
	if err != nil {
		pluginLogger.Err(err).Msg("Plugins are disabled because an error has occurred.")
	} else if hasPlugins {
		pluginLogger.Info().Msg("Plugins loaded.")
	}

	// Providers plugins
	// 根据Providers.Plugin中的配置，创建插件提供者，并存储到providerAggregator.Provider中
	for name, conf := range staticConfiguration.Providers.Plugin {
		if pluginBuilder == nil {
			break
		}

		// 根据插件名称和配置创建插件提供者
		p, err := pluginBuilder.BuildProvider(name, conf)
		if err != nil {
			return nil, fmt.Errorf("plugin: failed to build provider: %w", err)
		}

		// 将插件提供者存储到providerAggregator.Provider中
		err = providerAggregator.AddProvider(p)
		if err != nil {
			return nil, fmt.Errorf("plugin: failed to add provider: %w", err)
		}
	}

	// Service manager factory
	// 创建X509Source对象，用于获取SPIFFE证书
	var spiffeX509Source *workloadapi.X509Source
	if staticConfiguration.Spiffe != nil && staticConfiguration.Spiffe.WorkloadAPIAddr != "" {
		log.Info().Str("workloadAPIAddr", staticConfiguration.Spiffe.WorkloadAPIAddr).
			Msg("Waiting on SPIFFE SVID delivery")

		spiffeX509Source, err = workloadapi.NewX509Source(
			ctx,
			workloadapi.WithClientOptions(
				workloadapi.WithAddr(
					staticConfiguration.Spiffe.WorkloadAPIAddr,
				),
			),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create SPIFFE x509 source: %w", err)
		}
		log.Info().Msg("Successfully obtained SPIFFE SVID.")
	}

	// 创建证书通信传输层管理器，用于证书通信
	// 与spiffeX509Source一起使用，作为客户端证书的传输层
	transportManager := service.NewTransportManager(spiffeX509Source)

	// 创建一个反向代理构建器，如果Experimental.FastProxy不为空，则使用SmartBuilder
	// 这里最终使用反向代理构建器构建代理服务器与后端服务器进行通信
	var proxyBuilder service.ProxyBuilder = httputil.NewProxyBuilder(transportManager, semConvMetricRegistry)
	if staticConfiguration.Experimental != nil && staticConfiguration.Experimental.FastProxy != nil {
		proxyBuilder = proxy.NewSmartBuilder(transportManager, proxyBuilder, *staticConfiguration.Experimental.FastProxy)
	}

	// 创建一个给反向代理使用的Dial
	dialerManager := tcp.NewDialerManager(spiffeX509Source)

	// 创建acmeHTTPHandler，实际是httpChallengeProvider
	acmeHTTPHandler := getHTTPChallengeHandler(acmeProviders, httpChallengeProvider)
	// 创建服务管理工厂，存储了协程池、观察者管理器、传输管理器、反向代理构建器、acmeHTTPHandler、rest Handler、Ping Handler、Dashboard Handler、API Handler等
	// 如果配置存在Metrics，并且Metrics.Prometheus不为空，则创建PrometheusHandler
	//	所有的Handler都是标准的HTTP Handler
	managerFactory := service.NewManagerFactory(*staticConfiguration, routinesPool, observabilityMgr, transportManager, proxyBuilder, acmeHTTPHandler)

	// Router factory
	// RouterFactory是路由器工厂，用于创建路由器，里面包含了全部对请求要做的处理对象，包含了管理服务工厂，还包含了TCPEntryPoints和UDPEntryPoints的名称列表
	routerFactory := server.NewRouterFactory(*staticConfiguration, managerFactory, tlsManager, observabilityMgr, pluginBuilder, dialerManager)

	// Watcher
	// 创建一个配置观察者，用于监听配置的变化
	watcher := server.NewConfigurationWatcher(
		routinesPool,
		providerAggregator,
		getDefaultsEntrypoints(staticConfiguration),
		"internal",
	)

	// TLS
	// 将tlsManager的配置更新跟踪函数添加到观察者的configurationListeners中
	watcher.AddListener(func(conf dynamic.Configuration) {
		ctx := context.Background()
		//更新TLS配置
		tlsManager.UpdateConfigs(ctx, conf.TLS.Stores, conf.TLS.Options, conf.TLS.Certificates)

		gauge := metricsRegistry.TLSCertsNotAfterTimestampGauge()
		//遍历tlsManager中的所有证书
		for _, certificate := range tlsManager.GetServerCertificates() {
			// 记录证书的NotAfter时间(过期时间)
			// 这里的gauge是一个指标，用于记录证书的过期时间
			appendCertMetric(gauge, certificate)
		}
	})

	// Metrics
	// 将metricsRegistry的配置更新跟踪函数添加到观察者的configurationListeners中
	watcher.AddListener(func(_ dynamic.Configuration) {
		metricsRegistry.ConfigReloadsCounter().Add(1)
		metricsRegistry.LastConfigReloadSuccessGauge().Set(float64(time.Now().Unix()))
	})

	// Server Transports
	// 将transportManager、proxyBuilder、dialerManager的配置更新跟踪函数添加到观察者的configurationListeners中
	// 方便后续根据动态配置更新
	watcher.AddListener(func(conf dynamic.Configuration) {
		transportManager.Update(conf.HTTP.ServersTransports)
		proxyBuilder.Update(conf.HTTP.ServersTransports)
		dialerManager.Update(conf.TCP.ServersTransports)
	})

	// Switch router
	// 将路由器的配置更新跟踪函数添加到观察者的configurationListeners中
	watcher.AddListener(switchRouter(routerFactory, serverEntryPointsTCP, serverEntryPointsUDP))

	// Metrics
	if metricsRegistry.IsEpEnabled() || metricsRegistry.IsRouterEnabled() || metricsRegistry.IsSvcEnabled() {
		var eps []string
		for key := range serverEntryPointsTCP {
			eps = append(eps, key)
		}
		watcher.AddListener(func(conf dynamic.Configuration) {
			metrics.OnConfigurationUpdate(conf, eps)
		})
	}

	// TLS challenge
	watcher.AddListener(tlsChallengeProvider.ListenConfiguration)

	// Certificate Resolvers

	resolverNames := map[string]struct{}{}

	// ACME
	for _, p := range acmeProviders {
		resolverNames[p.ResolverName] = struct{}{}
		watcher.AddListener(p.ListenConfiguration)
	}

	// Tailscale
	for _, p := range tsProviders {
		resolverNames[p.ResolverName] = struct{}{}
		watcher.AddListener(p.HandleConfigUpdate)
	}

	// Certificate resolver logs
	watcher.AddListener(func(config dynamic.Configuration) {
		for rtName, rt := range config.HTTP.Routers {
			if rt.TLS == nil || rt.TLS.CertResolver == "" {
				continue
			}

			if _, ok := resolverNames[rt.TLS.CertResolver]; !ok {
				log.Error().Err(err).Str(logs.RouterName, rtName).Str("certificateResolver", rt.TLS.CertResolver).
					Msg("Router uses a nonexistent certificate resolver")
			}
		}
	})

	return server.NewServer(routinesPool, serverEntryPointsTCP, serverEntryPointsUDP, watcher, observabilityMgr), nil
}

func getHTTPChallengeHandler(acmeProviders []*acme.Provider, httpChallengeProvider http.Handler) http.Handler {
	var acmeHTTPHandler http.Handler
	for _, p := range acmeProviders {
		if p != nil && p.HTTPChallenge != nil {
			acmeHTTPHandler = httpChallengeProvider
			break
		}
	}
	return acmeHTTPHandler
}

func getDefaultsEntrypoints(staticConfiguration *static.Configuration) []string {
	var defaultEntryPoints []string

	// Determines if at least one EntryPoint is configured to be used by default.
	var hasDefinedDefaults bool
	for _, ep := range staticConfiguration.EntryPoints {
		if ep.AsDefault {
			hasDefinedDefaults = true
			break
		}
	}

	for name, cfg := range staticConfiguration.EntryPoints {
		// By default all entrypoints are considered.
		// If at least one is flagged, then only flagged entrypoints are included.
		if hasDefinedDefaults && !cfg.AsDefault {
			continue
		}

		protocol, err := cfg.GetProtocol()
		if err != nil {
			// Should never happen because Traefik should not start if protocol is invalid.
			log.Error().Err(err).Msg("Invalid protocol")
		}

		if protocol != "udp" && name != static.DefaultInternalEntryPointName {
			defaultEntryPoints = append(defaultEntryPoints, name)
		}
	}

	slices.Sort(defaultEntryPoints)
	return defaultEntryPoints
}

func switchRouter(routerFactory *server.RouterFactory, serverEntryPointsTCP server.TCPEntryPoints, serverEntryPointsUDP server.UDPEntryPoints) func(conf dynamic.Configuration) {
	// 这里返回的函数，会在执行configurationWatcher.Start() -> configurationWatcher.applyConfigurations()中，遍历configurationWatcher.configurationListeners成员中的函数时被调用
	// 这里传入的参数dynamic.Configuration时在configurationWatcher.Start() -> configurationWatcher.startProviderAggregator() -> provider.Provide()例如最终调用file.Provider.Provide()时，
	// 发送到configurationWatcher里的allProvidersConfigs成员，再由configurationWatcher.receiveConfigurations()方法中传递给newConfigs成员，然后再configurationWatcher.applyConfigurations()方法中使用
	return func(conf dynamic.Configuration) {
		// 创建runtime.Configuration，里面包含了HTTP、TCP、UDP和TLS的配置信息
		// 这里根据传入的dynamic.Configuration封装成runtime.Configuration，里面存储了HTTP、TCP、UDP的Router、Middleware和Service信息
		// 这三部分信息都经过了一层封装，封装成了RouterInfo,ServiceInfo,MiddlewareInfo
		rtConf := runtime.NewConfig(conf)

		//传入runtime.Configuration，然后生成tcp.Router和udp.Handler，之后将tcp.Router和UDP.Handler传入到
		routers, udpRouters := routerFactory.CreateRouters(rtConf)

		// 将routers和udpRouters中的handler添加到serverEntryPointsTCP和serverEntryPointsUDP的switcher中
		// 后续在TCPEntryPoint和UDPEntryPoint执行Start()的时候，实际上调用的是tcp.Router.ServeTCP()和udp.Handler.ServeUDP()
		serverEntryPointsTCP.Switch(routers)
		serverEntryPointsUDP.Switch(udpRouters)
	}
}

// initACMEProvider creates and registers acme.Provider instances corresponding to the configured ACME certificate resolvers.
func initACMEProvider(c *static.Configuration, providerAggregator *aggregator.ProviderAggregator, tlsManager *traefiktls.Manager, httpChallengeProvider, tlsChallengeProvider challenge.Provider, routinesPool *safe.Pool) []*acme.Provider {
	localStores := map[string]*acme.LocalStore{}

	var resolvers []*acme.Provider
	for name, resolver := range c.CertificatesResolvers {
		if resolver.ACME == nil {
			continue
		}

		if localStores[resolver.ACME.Storage] == nil {
			localStores[resolver.ACME.Storage] = acme.NewLocalStore(resolver.ACME.Storage, routinesPool)
		}

		p := &acme.Provider{
			Configuration:         resolver.ACME,
			Store:                 localStores[resolver.ACME.Storage],
			ResolverName:          name,
			HTTPChallengeProvider: httpChallengeProvider,
			TLSChallengeProvider:  tlsChallengeProvider,
		}

		if err := providerAggregator.AddProvider(p); err != nil {
			log.Error().Err(err).Str("resolver", name).Msg("The ACME resolve is skipped from the resolvers list")
			continue
		}

		p.SetTLSManager(tlsManager)

		p.SetConfigListenerChan(make(chan dynamic.Configuration))

		resolvers = append(resolvers, p)
	}

	return resolvers
}

// initTailscaleProviders creates and registers tailscale.Provider instances corresponding to the configured Tailscale certificate resolvers.
func initTailscaleProviders(cfg *static.Configuration, providerAggregator *aggregator.ProviderAggregator) []*tailscale.Provider {
	var providers []*tailscale.Provider
	for name, resolver := range cfg.CertificatesResolvers {
		if resolver.Tailscale == nil {
			continue
		}

		tsProvider := &tailscale.Provider{ResolverName: name}

		if err := providerAggregator.AddProvider(tsProvider); err != nil {
			log.Error().Err(err).Str(logs.ProviderName, name).Msg("Unable to create Tailscale provider")
			continue
		}

		providers = append(providers, tsProvider)
	}

	return providers
}

func registerMetricClients(metricsConfig *types.Metrics) []metrics.Registry {
	if metricsConfig == nil {
		return nil
	}

	var registries []metrics.Registry

	if metricsConfig.Prometheus != nil {
		logger := log.With().Str(logs.MetricsProviderName, "prometheus").Logger()

		prometheusRegister := metrics.RegisterPrometheus(logger.WithContext(context.Background()), metricsConfig.Prometheus)
		if prometheusRegister != nil {
			registries = append(registries, prometheusRegister)
			logger.Debug().Msg("Configured Prometheus metrics")
		}
	}

	if metricsConfig.Datadog != nil {
		logger := log.With().Str(logs.MetricsProviderName, "datadog").Logger()

		registries = append(registries, metrics.RegisterDatadog(logger.WithContext(context.Background()), metricsConfig.Datadog))
		logger.Debug().
			Str("address", metricsConfig.Datadog.Address).
			Str("pushInterval", metricsConfig.Datadog.PushInterval.String()).
			Msgf("Configured Datadog metrics")
	}

	if metricsConfig.StatsD != nil {
		logger := log.With().Str(logs.MetricsProviderName, "statsd").Logger()

		registries = append(registries, metrics.RegisterStatsd(logger.WithContext(context.Background()), metricsConfig.StatsD))
		logger.Debug().
			Str("address", metricsConfig.StatsD.Address).
			Str("pushInterval", metricsConfig.StatsD.PushInterval.String()).
			Msg("Configured StatsD metrics")
	}

	if metricsConfig.InfluxDB2 != nil {
		logger := log.With().Str(logs.MetricsProviderName, "influxdb2").Logger()

		influxDB2Register := metrics.RegisterInfluxDB2(logger.WithContext(context.Background()), metricsConfig.InfluxDB2)
		if influxDB2Register != nil {
			registries = append(registries, influxDB2Register)
			logger.Debug().
				Str("address", metricsConfig.InfluxDB2.Address).
				Str("bucket", metricsConfig.InfluxDB2.Bucket).
				Str("organization", metricsConfig.InfluxDB2.Org).
				Str("pushInterval", metricsConfig.InfluxDB2.PushInterval.String()).
				Msg("Configured InfluxDB v2 metrics")
		}
	}

	if metricsConfig.OTLP != nil {
		logger := log.With().Str(logs.MetricsProviderName, "openTelemetry").Logger()

		openTelemetryRegistry := metrics.RegisterOpenTelemetry(logger.WithContext(context.Background()), metricsConfig.OTLP)
		if openTelemetryRegistry != nil {
			registries = append(registries, openTelemetryRegistry)
			logger.Debug().
				Str("pushInterval", metricsConfig.OTLP.PushInterval.String()).
				Msg("Configured OpenTelemetry metrics")
		}
	}

	return registries
}

func appendCertMetric(gauge gokitmetrics.Gauge, certificate *x509.Certificate) {
	slices.Sort(certificate.DNSNames)

	labels := []string{
		"cn", certificate.Subject.CommonName,
		"serial", certificate.SerialNumber.String(),
		"sans", strings.Join(certificate.DNSNames, ","),
	}

	notAfter := float64(certificate.NotAfter.Unix())

	gauge.With(labels...).Set(notAfter)
}

func setupAccessLog(conf *types.AccessLog) *accesslog.Handler {
	if conf == nil {
		return nil
	}

	accessLoggerMiddleware, err := accesslog.NewHandler(conf)
	if err != nil {
		log.Warn().Err(err).Msg("Unable to create access logger")
		return nil
	}

	return accessLoggerMiddleware
}

func setupTracing(conf *static.Tracing) (*tracing.Tracer, io.Closer) {
	if conf == nil {
		return nil, nil
	}

	tracer, closer, err := tracing.NewTracing(conf)
	if err != nil {
		log.Warn().Err(err).Msg("Unable to create tracer")
		return nil, nil
	}

	return tracer, closer
}

func checkNewVersion() {
	ticker := time.Tick(24 * time.Hour)
	safe.Go(func() {
		for time.Sleep(10 * time.Minute); ; <-ticker {
			version.CheckNewVersion()
		}
	})
}

func stats(staticConfiguration *static.Configuration) {
	logger := log.With().Logger()

	if staticConfiguration.Global.SendAnonymousUsage {
		logger.Info().Msg(`Stats collection is enabled.`)
		logger.Info().Msg(`Many thanks for contributing to Traefik's improvement by allowing us to receive anonymous information from your configuration.`)
		logger.Info().Msg(`Help us improve Traefik by leaving this feature on :)`)
		logger.Info().Msg(`More details on: https://doc.traefik.io/traefik/contributing/data-collection/`)
		collect(staticConfiguration)
	} else {
		logger.Info().Msg(`
Stats collection is disabled.
Help us improve Traefik by turning this feature on :)
More details on: https://doc.traefik.io/traefik/contributing/data-collection/
`)
	}
}

func collect(staticConfiguration *static.Configuration) {
	ticker := time.Tick(24 * time.Hour)
	safe.Go(func() {
		for time.Sleep(10 * time.Minute); ; <-ticker {
			if err := collector.Collect(staticConfiguration); err != nil {
				log.Debug().Err(err).Send()
			}
		}
	})
}
