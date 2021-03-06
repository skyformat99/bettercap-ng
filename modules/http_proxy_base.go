package modules

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/evilsocket/bettercap-ng/core"
	"github.com/evilsocket/bettercap-ng/firewall"
	"github.com/evilsocket/bettercap-ng/log"
	"github.com/evilsocket/bettercap-ng/session"
	btls "github.com/evilsocket/bettercap-ng/tls"

	"github.com/elazarl/goproxy"
	"github.com/inconshreveable/go-vhost"
)

type HTTPProxy struct {
	Name        string
	Address     string
	Server      http.Server
	Redirection *firewall.Redirection
	Proxy       *goproxy.ProxyHttpServer
	Script      *ProxyScript
	CertFile    string
	KeyFile     string

	isTLS       bool
	isRunning   bool
	sniListener net.Listener
	sess        *session.Session
}

func stripPort(s string) string {
	ix := strings.IndexRune(s, ':')
	if ix == -1 {
		return s
	}
	return s[:ix]
}

func NewHTTPProxy(s *session.Session) *HTTPProxy {
	p := &HTTPProxy{
		Name:  "http.proxy",
		Proxy: goproxy.NewProxyHttpServer(),
		sess:  s,
		isTLS: false,
	}

	p.Proxy.NonproxyHandler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if p.doProxy(req) == true {
			req.URL.Host = req.Host
			p.Proxy.ServeHTTP(w, req)
		}
	})

	p.Proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	p.Proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		log.Debug("(%s) < %s %s %s%s", core.Green(p.Name), req.RemoteAddr, req.Method, req.Host, req.URL.Path)
		if p.Script != nil {
			jsres := p.Script.OnRequest(req)
			if jsres != nil {
				p.logAction(req, jsres)
				return req, jsres.ToResponse(req)
			}
		}
		return req, nil
	})

	p.Proxy.OnResponse().DoFunc(func(res *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		req := res.Request
		log.Debug("(%s) > %s %s %s%s", core.Green(p.Name), req.RemoteAddr, req.Method, req.Host, req.URL.Path)
		if p.Script != nil {
			jsres := p.Script.OnResponse(res)
			if jsres != nil {
				p.logAction(res.Request, jsres)
				return jsres.ToResponse(res.Request)
			}
		}
		return res
	})

	return p
}

func (p *HTTPProxy) logAction(req *http.Request, jsres *JSResponse) {
	p.sess.Events.Add(p.Name+".spoofed-response", struct {
		To     string
		Method string
		Host   string
		Path   string
		Size   int
	}{
		strings.Split(req.RemoteAddr, ":")[0],
		req.Method,
		req.Host,
		req.URL.Path,
		len(jsres.Body),
	})
}

func (p *HTTPProxy) doProxy(req *http.Request) bool {
	blacklist := []string{
		"localhost",
		"127.0.0.1",
	}

	if req.Host == "" {
		log.Error("Got request with empty host: %v", req)
		return false
	}

	for _, blacklisted := range blacklist {
		if strings.HasPrefix(req.Host, blacklisted) {
			log.Error("Got request with blacklisted host: %s", req.Host)
			return false
		}
	}

	return true
}

func (p *HTTPProxy) Configure(address string, proxyPort int, httpPort int, scriptPath string) error {
	var err error

	p.Address = address

	if scriptPath != "" {
		if err, p.Script = LoadProxyScript(scriptPath, p.sess); err != nil {
			return err
		} else {
			log.Debug("Proxy script %s loaded.", scriptPath)
		}
	}

	p.Server = http.Server{
		Addr:    fmt.Sprintf("%s:%d", p.Address, proxyPort),
		Handler: p.Proxy,
	}

	if p.sess.Firewall.IsForwardingEnabled() == false {
		log.Info("Enabling forwarding.")
		p.sess.Firewall.EnableForwarding(true)
	}

	p.Redirection = firewall.NewRedirection(p.sess.Interface.Name(),
		"TCP",
		httpPort,
		p.Address,
		proxyPort)

	if err := p.sess.Firewall.EnableRedirection(p.Redirection, true); err != nil {
		return err
	}

	log.Debug("Applied redirection %s", p.Redirection.String())

	return nil
}

func TLSConfigFromCA(ca *tls.Certificate) func(host string, ctx *goproxy.ProxyCtx) (*tls.Config, error) {
	return func(host string, ctx *goproxy.ProxyCtx) (c *tls.Config, err error) {
		parts := strings.SplitN(host, ":", 2)
		hostname := parts[0]
		port := 443
		if len(parts) > 1 {
			port, err = strconv.Atoi(parts[1])
			if err != nil {
				port = 443
			}
		}

		cert := getCachedCert(hostname, port)
		if cert == nil {
			log.Info("Creating spoofed certificate for %s:%d", core.Yellow(hostname), port)
			cert, err = btls.SignCertificateForHost(ca, hostname, port)
			if err != nil {
				log.Warning("Cannot sign host certificate with provided CA: %s", err)
				return nil, err
			}

			setCachedCert(hostname, port, cert)
		}

		config := tls.Config{
			InsecureSkipVerify: true,
			Certificates:       []tls.Certificate{*cert},
		}

		return &config, nil
	}
}

func (p *HTTPProxy) ConfigureTLS(address string, proxyPort int, httpPort int, scriptPath string, certFile string, keyFile string) error {
	err := p.Configure(address, proxyPort, httpPort, scriptPath)
	if err != nil {
		return err
	}

	p.isTLS = true
	p.Name = "https.proxy"
	p.CertFile = certFile
	p.KeyFile = keyFile

	rawCert, _ := ioutil.ReadFile(p.CertFile)
	rawKey, _ := ioutil.ReadFile(p.KeyFile)

	ourCa, err := tls.X509KeyPair(rawCert, rawKey)
	if err != nil {
		return err
	}

	if ourCa.Leaf, err = x509.ParseCertificate(ourCa.Certificate[0]); err != nil {
		return err
	}

	goproxy.GoproxyCa = ourCa
	goproxy.OkConnect = &goproxy.ConnectAction{Action: goproxy.ConnectAccept, TLSConfig: TLSConfigFromCA(&ourCa)}
	goproxy.MitmConnect = &goproxy.ConnectAction{Action: goproxy.ConnectMitm, TLSConfig: TLSConfigFromCA(&ourCa)}
	goproxy.HTTPMitmConnect = &goproxy.ConnectAction{Action: goproxy.ConnectHTTPMitm, TLSConfig: TLSConfigFromCA(&ourCa)}
	goproxy.RejectConnect = &goproxy.ConnectAction{Action: goproxy.ConnectReject, TLSConfig: TLSConfigFromCA(&ourCa)}

	return nil
}

func (p *HTTPProxy) httpWorker() error {
	p.isRunning = true
	return p.Server.ListenAndServe()
}

type dumbResponseWriter struct {
	net.Conn
}

func (dumb dumbResponseWriter) Header() http.Header {
	panic("Header() should not be called on this ResponseWriter")
}

func (dumb dumbResponseWriter) Write(buf []byte) (int, error) {
	if bytes.Equal(buf, []byte("HTTP/1.0 200 OK\r\n\r\n")) {
		return len(buf), nil // throw away the HTTP OK response from the faux CONNECT request
	}
	return dumb.Conn.Write(buf)
}

func (dumb dumbResponseWriter) WriteHeader(code int) {
	panic("WriteHeader() should not be called on this ResponseWriter")
}

func (dumb dumbResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return dumb, bufio.NewReadWriter(bufio.NewReader(dumb), bufio.NewWriter(dumb)), nil
}

func (p *HTTPProxy) httpsWorker() error {
	var err error

	// listen to the TLS ClientHello but make it a CONNECT request instead
	p.sniListener, err = net.Listen("tcp", p.Server.Addr)
	if err != nil {
		return err
	}

	p.isRunning = true
	for p.isRunning {
		c, err := p.sniListener.Accept()
		if err != nil {
			log.Warning("Error accepting connection: %s.", err)
			continue
		}

		go func(c net.Conn) {
			tlsConn, err := vhost.TLS(c)
			if err != nil {
				log.Warning("Error reading SNI: %s.", err)
				return
			}

			hostname := tlsConn.Host()
			if hostname == "" {
				log.Warning("Client does not support SNI.")
				return
			}

			log.Debug("Got new SNI from %s for %s", core.Bold(stripPort(c.RemoteAddr().String())), core.Yellow(hostname))

			req := &http.Request{
				Method: "CONNECT",
				URL: &url.URL{
					Opaque: hostname,
					Host:   net.JoinHostPort(hostname, "443"),
				},
				Host:   hostname,
				Header: make(http.Header),
			}
			resp := dumbResponseWriter{tlsConn}
			p.Proxy.ServeHTTP(resp, req)
		}(c)
	}

	return nil
}

func (p *HTTPProxy) Start() {
	go func() {
		var err error

		if p.isTLS == true {
			err = p.httpsWorker()
		} else {
			err = p.httpWorker()
		}

		if err != nil {
			log.Warning("%s", err)
		}
	}()
}

func (p *HTTPProxy) Stop() error {
	if p.Redirection != nil {
		log.Debug("Disabling redirection %s", p.Redirection.String())
		if err := p.sess.Firewall.EnableRedirection(p.Redirection, false); err != nil {
			return err
		}
		p.Redirection = nil
	}

	if p.isTLS == true {
		p.isRunning = false
		p.sniListener.Close()
		return nil
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return p.Server.Shutdown(ctx)
	}
}
