//go:build xray && sing

package core_test

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"os/exec"
	"testing"
	"time"

	"encoding/json"

	"github.com/InazumaV/V2bX/api/panel"
	"github.com/InazumaV/V2bX/conf"
	vcore "github.com/InazumaV/V2bX/core"
	_ "github.com/InazumaV/V2bX/core/sing"
	_ "github.com/InazumaV/V2bX/core/xray"
	"github.com/InazumaV/V2bX/limiter"
	"github.com/google/uuid"
	coreConf "github.com/xtls/xray-core/infra/conf"
	xnet "golang.org/x/net/proxy"

	xcore "github.com/xtls/xray-core/core"
)

func TestVLESSRealityVision(t *testing.T) {
	for _, coreName := range []string{"xray", "sing"} {
		t.Run(coreName, func(t *testing.T) {
			testVLESSRealityVision(t, coreName)
		})
	}
}

func testVLESSRealityVision(t *testing.T, coreName string) {
	t.Helper()
	destLn := startTLSServer(t)
	destPort := destLn.Addr().(*net.TCPAddr).Port
	const echoIP = "192.0.0.20"
	ensureLoopbackAlias(t, echoIP)
	echoLn := startTCPEcho(t, echoIP)
	echoPort := echoLn.Addr().(*net.TCPAddr).Port

	priv, pub := realityKeypair(t)
	const shortID = "0123456789abcdef"
	const serverName = "example.com"
	userID := uuid.NewString()
	const panelUID = 7

	serverPort := freePort(t)
	socksPort := freePort(t)

	node := &panel.NodeInfo{
		Type:     "vless",
		Security: panel.Reality,
		Common: &panel.CommonNode{
			ServerPort: serverPort,
		},
		VAllss: &panel.VAllssNode{
			Network:         "tcp",
			NetworkSettings: json.RawMessage(`{}`),
			Flow:            "xtls-rprx-vision",
			TlsSettings: panel.TlsSettings{
				ServerName: serverName,
				Dest:       "127.0.0.1",
				ServerPort: fmt.Sprintf("%d", destPort),
				ShortId:    shortID,
				PrivateKey: priv,
			},
		},
	}
	options := &conf.Options{
		ListenIP:         "127.0.0.1",
		ReportMinTraffic: 0,
		XrayOptions:      conf.NewXrayOptions(),
		SingOptions:      conf.NewSingOptions(),
	}
	options.XrayOptions.DisableSniffing = true

	server, err := vcore.NewCore([]conf.CoreConfig{{
		Type:       coreName,
		XrayConfig: conf.NewXrayConfig(),
		SingConfig: conf.NewSingConfig(),
	}})
	if err != nil {
		t.Fatalf("new core: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err = server.Start(); err != nil {
		t.Fatalf("start core: %v", err)
	}
	if err = server.AddNode("reality-test", node, options); err != nil {
		t.Fatalf("add node: %v", err)
	}
	added, err := server.AddUsers(&vcore.AddUsersParams{
		Tag:      "reality-test",
		NodeInfo: node,
		Users: []panel.UserInfo{{
			Id:   panelUID,
			Uuid: userID,
		}},
	})
	if err != nil {
		t.Fatalf("add users: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	limiter.Init()
	limiter.AddLimiter("reality-test", &conf.LimitConfig{}, []panel.UserInfo{{
		Id:   panelUID,
		Uuid: userID,
	}}, nil)
	t.Cleanup(func() { limiter.DeleteLimiter("reality-test") })

	client := startXrayClient(t, socksPort, serverPort, userID, pub, shortID, serverName)
	t.Cleanup(func() { _ = client.Close() })

	payload := []byte("vless-reality-vision-ping")
	if err = socksEcho(socksPort, echoIP, echoPort, payload); err != nil {
		t.Fatalf("vision exchange: %v", err)
	}

	traffic := waitTraffic(t, server, panelUID)
	if traffic.Upload == 0 || traffic.Download == 0 {
		t.Fatalf("traffic upload=%d download=%d", traffic.Upload, traffic.Download)
	}

	if err = server.DelUsers([]panel.UserInfo{{Id: panelUID, Uuid: userID}}, "reality-test", node); err != nil {
		t.Fatalf("del users: %v", err)
	}
	if err = socksEcho(socksPort, echoIP, echoPort, payload); err == nil {
		t.Fatal("connection succeeded after user removal")
	}
}

func ensureLoopbackAlias(t *testing.T, ip string) {
	t.Helper()
	if out, err := exec.Command("ip", "addr", "replace", ip+"/32", "dev", "lo").CombinedOutput(); err != nil {
		t.Fatalf("add %s on lo: %v: %s", ip, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "addr", "del", ip+"/32", "dev", "lo").Run()
	})
}

func startTCPEcho(t *testing.T, ip string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln
}

func startTLSServer(t *testing.T) net.Listener {
	t.Helper()
	cert, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSConfig(t))
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	t.Cleanup(func() { _ = cert.Close() })
	go func() {
		for {
			conn, err := cert.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return cert
}

func serverTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("p256: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"example.com"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}}}
}

func realityKeypair(t *testing.T) (privateKey, publicKey string) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("x25519: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(key.Bytes()),
		base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func startXrayClient(t *testing.T, socksPort, serverPort int, userID, publicKey, shortID, serverName string) *xcore.Instance {
	t.Helper()
	raw := fmt.Sprintf(`{
		"log": {"loglevel": "warning"},
		"inbounds": [{
			"listen": "127.0.0.1",
			"port": %d,
			"protocol": "socks",
			"settings": {"udp": false}
		}],
		"outbounds": [{
			"protocol": "vless",
			"settings": {
				"vnext": [{
					"address": "127.0.0.1",
					"port": %d,
					"users": [{
						"id": %q,
						"encryption": "none",
						"flow": "xtls-rprx-vision"
					}]
				}]
			},
			"streamSettings": {
				"network": "tcp",
				"security": "reality",
				"realitySettings": {
					"fingerprint": "chrome",
					"serverName": %q,
					"publicKey": %q,
					"shortId": %q
				}
			}
		}]
	}`, socksPort, serverPort, userID, serverName, publicKey, shortID)
	cfg := &coreConf.Config{}
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		t.Fatalf("unmarshal client: %v", err)
	}
	built, err := cfg.Build()
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	instance, err := xcore.New(built)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err = instance.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	return instance
}

func socksEcho(socksPort int, destIP string, destPort int, payload []byte) error {
	dialer, err := xnet.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", socksPort), nil, xnet.Direct)
	if err != nil {
		return err
	}
	conn, err := dialer.Dial("tcp", net.JoinHostPort(destIP, fmt.Sprintf("%d", destPort)))
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = conn.Write(payload); err != nil {
		return err
	}
	buf := make([]byte, len(payload))
	if _, err = io.ReadFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != string(payload) {
		return fmt.Errorf("echo %q", buf)
	}
	return nil
}

func waitTraffic(t *testing.T, server vcore.Core, uid int) panel.UserTraffic {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		report, err := server.GetUserTrafficSlice("reality-test", false)
		if err != nil {
			t.Fatalf("traffic: %v", err)
		}
		for _, item := range report {
			if item.UID == uid && item.Upload > 0 && item.Download > 0 {
				return item
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no traffic reported: %+v", report)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
