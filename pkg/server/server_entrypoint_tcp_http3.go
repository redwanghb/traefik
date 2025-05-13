package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/config/static"
	tcprouter "github.com/traefik/traefik/v3/pkg/server/router/tcp"
)

type http3server struct {
	*http3.Server

	http3conn net.PacketConn

	lock   sync.RWMutex
	getter func(info *tls.ClientHelloInfo) (*tls.Config, error)
}

func newHTTP3Server(ctx context.Context, name string, config *static.EntryPoint, httpsServer *httpServer) (*http3server, error) {
	var conn net.PacketConn
	var err error

	if config.HTTP3 == nil {
		return nil, nil
	}

	if config.HTTP3.AdvertisedPort < 0 {
		return nil, errors.New("advertised port must be greater than or equal to zero")
	}

	// if we have predefined connections from socket activation
	// 判定是否启用了socketActivation功能，socketActivation是利用了go-systemd库里的activation.Files()方法，
	// 相当于是通过systemd的socket激活功能来获取预定义的连接
	if socketActivation.isEnabled() {
		conn, err = socketActivation.getConn(name)
		if err != nil {
			log.Ctx(ctx).Warn().Err(err).Str("name", name).Msg("Unable to use socket activation for entrypoint")
		}
	}

	// 如果没有找到预定义的连接，则使用ListenConfig来创建一个新的连接
	if conn == nil {
		listenConfig := newListenConfig(config)
		conn, err = listenConfig.ListenPacket(ctx, "udp", config.GetAddress())
		if err != nil {
			return nil, fmt.Errorf("starting listener: %w", err)
		}
	}

	h3 := &http3server{
		http3conn: conn,
		getter: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			return nil, errors.New("no tls config")
		},
	}

	// 复用了httpsServer的Server对象中的Handler
	h3.Server = &http3.Server{
		Addr:    config.GetAddress(),
		Port:    config.HTTP3.AdvertisedPort,
		Handler: httpsServer.Server.(*http.Server).Handler,
		// 配置文件中设置的GetConfigForClient实际上也是调用的h3.getter的函数
		TLSConfig: &tls.Config{GetConfigForClient: h3.getGetConfigForClient},
		// 禁用0-RTT(0-RTT是QUIC协议中的一个特性，允许客户端在第一次握手时发送数据)
		QUICConfig: &quic.Config{
			Allow0RTT: false,
		},
	}

	previousHandler := httpsServer.Server.(*http.Server).Handler

	// 封装httpsServer.Server.Handler，额外增加设置QUIC头部，这里会修改httpsServer.Server.Handler
	httpsServer.Server.(*http.Server).Handler = http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if err := h3.Server.SetQUICHeaders(rw.Header()); err != nil {
			log.Ctx(ctx).Error().Err(err).Msg("Failed to set HTTP3 headers")
		}

		previousHandler.ServeHTTP(rw, req)
	})

	return h3, nil
}

func (e *http3server) Start() error {
	return e.Serve(e.http3conn)
}

func (e *http3server) Switch(rt *tcprouter.Router) {
	e.lock.Lock()
	defer e.lock.Unlock()

	e.getter = rt.GetTLSGetClientInfo()
}

func (e *http3server) getGetConfigForClient(info *tls.ClientHelloInfo) (*tls.Config, error) {
	e.lock.RLock()
	defer e.lock.RUnlock()

	return e.getter(info)
}

func (e *http3server) Shutdown(_ context.Context) error {
	// TODO: use e.Server.CloseGracefully() when available.
	return e.Server.Close()
}
