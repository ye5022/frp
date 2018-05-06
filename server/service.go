// Copyright 2017 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"github.com/fatedier/frp/assets"
	"github.com/fatedier/frp/g"
	"github.com/fatedier/frp/models/msg"
	"github.com/fatedier/frp/utils/log"
	frpNet "github.com/fatedier/frp/utils/net"
	"github.com/fatedier/frp/utils/net/mux"
	"github.com/fatedier/frp/utils/util"
	"github.com/fatedier/frp/utils/version"
	"github.com/fatedier/frp/utils/vhost"

	fmux "github.com/hashicorp/yamux"
)

const (
	connReadTimeout time.Duration = 10 * time.Second
)

var ServerService *Service

// Server service.
type Service struct {
	// Dispatch connections to different handlers listen on same port.
	muxer *mux.Mux

	// Accept connections from client.
	listener frpNet.Listener

	// Accept connections using kcp.
	kcpListener frpNet.Listener

	// For https proxies, route requests to different clients by hostname and other infomation.
	VhostHttpsMuxer *vhost.HttpsMuxer

	httpReverseProxy *vhost.HttpReverseProxy

	// Manage all controllers.
	ctlManager *ControlManager

	// Manage all proxies.
	pxyManager *ProxyManager

	// Manage all visitor listeners.
	visitorManager *VisitorManager

	// Manage all tcp ports.
	tcpPortManager *PortManager

	// Manage all udp ports.
	udpPortManager *PortManager

	// Controller for nat hole connections.
	natHoleController *NatHoleController
}

func NewService() (svr *Service, err error) {
	cfg := &g.GlbServerCfg.ServerCommonConf
	svr = &Service{
		ctlManager:     NewControlManager(),
		pxyManager:     NewProxyManager(),
		visitorManager: NewVisitorManager(),
		tcpPortManager: NewPortManager("tcp", cfg.ProxyBindAddr, cfg.AllowPorts),
		udpPortManager: NewPortManager("udp", cfg.ProxyBindAddr, cfg.AllowPorts),
	}

	// Init assets.
	err = assets.Load(cfg.AssetsDir)
	if err != nil {
		err = fmt.Errorf("Load assets error: %v", err)
		return
	}

	var (
		httpMuxOn  bool
		httpsMuxOn bool
	)
	if cfg.BindAddr == cfg.ProxyBindAddr {
		if cfg.BindPort == cfg.VhostHttpPort {
			httpMuxOn = true
		}
		if cfg.BindPort == cfg.VhostHttpsPort {
			httpsMuxOn = true
		}
		if httpMuxOn || httpsMuxOn {
			svr.muxer = mux.NewMux()
		}
	}

	// Listen for accepting connections from client.
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.BindPort))
	if err != nil {
		err = fmt.Errorf("Create server listener error, %v", err)
		return
	}
	if svr.muxer != nil {
		go svr.muxer.Serve(ln)
		ln = svr.muxer.DefaultListener()
	}
	svr.listener = frpNet.WrapLogListener(ln)
	log.Info("frps tcp listen on %s:%d", cfg.BindAddr, cfg.BindPort)

	// Listen for accepting connections from client using kcp protocol.
	if cfg.KcpBindPort > 0 {
		svr.kcpListener, err = frpNet.ListenKcp(cfg.BindAddr, cfg.KcpBindPort)
		if err != nil {
			err = fmt.Errorf("Listen on kcp address udp [%s:%d] error: %v", cfg.BindAddr, cfg.KcpBindPort, err)
			return
		}
		log.Info("frps kcp listen on udp %s:%d", cfg.BindAddr, cfg.KcpBindPort)
	}

	// Create http vhost muxer.
	if cfg.VhostHttpPort > 0 {
		rp := vhost.NewHttpReverseProxy()
		svr.httpReverseProxy = rp

		address := fmt.Sprintf("%s:%d", cfg.ProxyBindAddr, cfg.VhostHttpPort)
		server := &http.Server{
			Addr:    address,
			Handler: rp,
		}
		var l net.Listener
		if httpMuxOn {
			l = svr.muxer.ListenHttp(0)
		} else {
			l, err = net.Listen("tcp", address)
			if err != nil {
				err = fmt.Errorf("Create vhost http listener error, %v", err)
				return
			}
		}
		go server.Serve(l)
		log.Info("http service listen on %s:%d", cfg.ProxyBindAddr, cfg.VhostHttpPort)
	}

	// Create https vhost muxer.
	if cfg.VhostHttpsPort > 0 {
		var l net.Listener
		if httpsMuxOn {
			l = svr.muxer.ListenHttps(0)
		} else {
			l, err = net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.ProxyBindAddr, cfg.VhostHttpsPort))
			if err != nil {
				err = fmt.Errorf("Create server listener error, %v", err)
				return
			}
		}

		svr.VhostHttpsMuxer, err = vhost.NewHttpsMuxer(frpNet.WrapLogListener(l), 30*time.Second)
		if err != nil {
			err = fmt.Errorf("Create vhost httpsMuxer error, %v", err)
			return
		}
		log.Info("https service listen on %s:%d", cfg.ProxyBindAddr, cfg.VhostHttpsPort)
	}

	// Create nat hole controller.
	if cfg.BindUdpPort > 0 {
		var nc *NatHoleController
		addr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.BindUdpPort)
		nc, err = NewNatHoleController(addr)
		if err != nil {
			err = fmt.Errorf("Create nat hole controller error, %v", err)
			return
		}
		svr.natHoleController = nc
		log.Info("nat hole udp service listen on %s:%d", cfg.BindAddr, cfg.BindUdpPort)
	}

	// Create dashboard web server.
	if cfg.DashboardPort > 0 {
		err = RunDashboardServer(cfg.DashboardAddr, cfg.DashboardPort)
		if err != nil {
			err = fmt.Errorf("Create dashboard web server error, %v", err)
			return
		}
		log.Info("Dashboard listen on %s:%d", cfg.DashboardAddr, cfg.DashboardPort)
	}
	return
}

func (svr *Service) Run() {
	if svr.natHoleController != nil {
		go svr.natHoleController.Run()
	}
	if g.GlbServerCfg.KcpBindPort > 0 {
		go svr.HandleListener(svr.kcpListener)
	}
	svr.HandleListener(svr.listener)

}

func (svr *Service) HandleListener(l frpNet.Listener) {
	// Listen for incoming connections from client.
	for {
		c, err := l.Accept()
		if err != nil {
			log.Warn("Listener for incoming connections from client closed")
			return
		}

		// Start a new goroutine for dealing connections.
		go func(frpConn frpNet.Conn) {
			dealFn := func(conn frpNet.Conn) {
				var rawMsg msg.Message
				conn.SetReadDeadline(time.Now().Add(connReadTimeout))
				if rawMsg, err = msg.ReadMsg(conn); err != nil {
					log.Trace("Failed to read message: %v", err)
					conn.Close()
					return
				}
				conn.SetReadDeadline(time.Time{})

				switch m := rawMsg.(type) {
				case *msg.Login:
					err = svr.RegisterControl(conn, m)
					// If login failed, send error message there.
					// Otherwise send success message in control's work goroutine.
					if err != nil {
						conn.Warn("%v", err)
						msg.WriteMsg(conn, &msg.LoginResp{
							Version: version.Full(),
							Error:   err.Error(),
						})
						conn.Close()
					}
				case *msg.NewWorkConn:
					svr.RegisterWorkConn(conn, m)
				case *msg.NewVisitorConn:
					if err = svr.RegisterVisitorConn(conn, m); err != nil {
						conn.Warn("%v", err)
						msg.WriteMsg(conn, &msg.NewVisitorConnResp{
							ProxyName: m.ProxyName,
							Error:     err.Error(),
						})
						conn.Close()
					} else {
						msg.WriteMsg(conn, &msg.NewVisitorConnResp{
							ProxyName: m.ProxyName,
							Error:     "",
						})
					}
				default:
					log.Warn("Error message type for the new connection [%s]", conn.RemoteAddr().String())
					conn.Close()
				}
			}

			if g.GlbServerCfg.TcpMux {
				fmuxCfg := fmux.DefaultConfig()
				fmuxCfg.LogOutput = ioutil.Discard
				session, err := fmux.Server(frpConn, fmuxCfg)
				if err != nil {
					log.Warn("Failed to create mux connection: %v", err)
					frpConn.Close()
					return
				}

				for {
					stream, err := session.AcceptStream()
					if err != nil {
						log.Warn("Accept new mux stream error: %v", err)
						session.Close()
						return
					}
					wrapConn := frpNet.WrapConn(stream)
					go dealFn(wrapConn)
				}
			} else {
				dealFn(frpConn)
			}
		}(c)
	}
}

func (svr *Service) RegisterControl(ctlConn frpNet.Conn, loginMsg *msg.Login) (err error) {
	ctlConn.Info("client login info: ip [%s] version [%s] hostname [%s] os [%s] arch [%s]",
		ctlConn.RemoteAddr().String(), loginMsg.Version, loginMsg.Hostname, loginMsg.Os, loginMsg.Arch)

	// Check client version.
	if ok, msg := version.Compat(loginMsg.Version); !ok {
		err = fmt.Errorf("%s", msg)
		return
	}

	// Check auth.
	nowTime := time.Now().Unix()
	if g.GlbServerCfg.AuthTimeout != 0 && nowTime-loginMsg.Timestamp > g.GlbServerCfg.AuthTimeout {
		err = fmt.Errorf("authorization timeout")
		return
	}
	if util.GetAuthKey(g.GlbServerCfg.Token, loginMsg.Timestamp) != loginMsg.PrivilegeKey {
		err = fmt.Errorf("authorization failed")
		return
	}

	// If client's RunId is empty, it's a new client, we just create a new controller.
	// Otherwise, we check if there is one controller has the same run id. If so, we release previous controller and start new one.
	if loginMsg.RunId == "" {
		loginMsg.RunId, err = util.RandId()
		if err != nil {
			return
		}
	}

	ctl := NewControl(svr, ctlConn, loginMsg)

	if oldCtl := svr.ctlManager.Add(loginMsg.RunId, ctl); oldCtl != nil {
		oldCtl.allShutdown.WaitDone()
	}

	ctlConn.AddLogPrefix(loginMsg.RunId)
	ctl.Start()

	// for statistics
	StatsNewClient()
	return
}

// RegisterWorkConn register a new work connection to control and proxies need it.
func (svr *Service) RegisterWorkConn(workConn frpNet.Conn, newMsg *msg.NewWorkConn) {
	ctl, exist := svr.ctlManager.GetById(newMsg.RunId)
	if !exist {
		workConn.Warn("No client control found for run id [%s]", newMsg.RunId)
		return
	}
	ctl.RegisterWorkConn(workConn)
	return
}

func (svr *Service) RegisterVisitorConn(visitorConn frpNet.Conn, newMsg *msg.NewVisitorConn) error {
	return svr.visitorManager.NewConn(newMsg.ProxyName, visitorConn, newMsg.Timestamp, newMsg.SignKey,
		newMsg.UseEncryption, newMsg.UseCompression)
}

func (svr *Service) RegisterProxy(name string, pxy Proxy) error {
	return svr.pxyManager.Add(name, pxy)
}

func (svr *Service) DelProxy(name string) {
	svr.pxyManager.Del(name)
}
