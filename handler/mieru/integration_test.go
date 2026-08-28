package mieru_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-gost/core/logger"
	"github.com/go-gost/x/config"
	service_parser "github.com/go-gost/x/config/parsing/service"
	_ "github.com/go-gost/x/handler/mieru"
	_ "github.com/go-gost/x/listener/mieru"
	xlogger "github.com/go-gost/x/logger"
)

func TestMain(m *testing.M) {
	logger.SetDefault(xlogger.NewLogger(xlogger.LevelOption(logger.ErrorLevel)))
	os.Exit(m.Run())
}

func TestOfficialMieruClient(t *testing.T) {
	mieruBin, err := exec.LookPath("mieru")
	if err != nil {
		t.Skip("mieru client not found in PATH")
	}

	httpPort := freeTCPPort(t)
	mieruPort := freeTCPPort(t)
	rpcPort := freeTCPPort(t)
	socksPort := freeTCPPort(t)

	httpLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { httpLn.Close() })
	httpSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	go httpSrv.Serve(httpLn)
	t.Cleanup(func() { httpSrv.Close() })
	waitTCP(t, httpPort)

	svc, err := service_parser.ParseService(&config.ServiceConfig{
		Name: fmt.Sprintf("mieru-test-%d", mieruPort),
		Addr: fmt.Sprintf(":%d", mieruPort),
		Listener: &config.ListenerConfig{
			Type: "mieru",
			Metadata: map[string]any{
				"users": map[string]any{
					"testuser": "testpass",
				},
				"mtu": 1400,
			},
		},
		Handler: &config.HandlerConfig{
			Type: "mieru",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	errc := make(chan error, 1)
	go func() { errc <- svc.Serve() }()
	t.Cleanup(func() { svc.Close() })
	waitTCP(t, mieruPort)

	cfgPath := filepath.Join(t.TempDir(), "client.json")
	cfg := map[string]any{
		"profiles": []any{
			map[string]any{
				"profileName": "default",
				"user": map[string]any{
					"name":     "testuser",
					"password": "testpass",
				},
				"servers": []any{
					map[string]any{
						"ipAddress": "127.0.0.1",
						"portBindings": []any{
							map[string]any{
								"port":     mieruPort,
								"protocol": "TCP",
							},
						},
					},
				},
				"mtu": 1400,
				"multiplexing": map[string]any{
					"level": "MULTIPLEXING_LOW",
				},
			},
		},
		"activeProfile": "default",
		"rpcPort":       rpcPort,
		"socks5Port":    socksPort,
		"loggingLevel":  "ERROR",
		"advancedSettings": map[string]any{
			"noCheckUpdate": true,
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	runMieru(t, mieruBin, "stop")
	t.Cleanup(func() { runMieru(t, mieruBin, "stop") })

	if out, err := exec.Command(mieruBin, "apply", "config", cfgPath).CombinedOutput(); err != nil {
		t.Fatalf("mieru apply config: %v\n%s", err, out)
	}
	startCmd := exec.Command(mieruBin, "start")
	startCmd.Stdout = os.Stdout
	startCmd.Stderr = os.Stderr
	if err := startCmd.Run(); err != nil {
		t.Fatalf("mieru start: %v", err)
	}
	waitTCP(t, socksPort)

	testURL := fmt.Sprintf("http://127.0.0.1:%d/", httpPort)
	if out, err := exec.Command(mieruBin, "test", testURL).CombinedOutput(); err != nil {
		t.Fatalf("mieru test %s: %v\n%s", testURL, err, out)
	}
	t.Logf("mieru client test succeeded via socks5://127.0.0.1:%d -> %s", socksPort, testURL)

	select {
	case err := <-errc:
		if err != nil && !isClosed(err) {
			t.Fatalf("service exited: %v", err)
		}
	default:
	}
}

func TestOfficialMieruClientUDPTransport(t *testing.T) {
	mieruBin, err := exec.LookPath("mieru")
	if err != nil {
		t.Skip("mieru client not found in PATH")
	}

	httpPort := freeTCPPort(t)
	mieruPort := freeUDPPort(t)
	rpcPort := freeTCPPort(t)
	socksPort := freeTCPPort(t)

	httpLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { httpLn.Close() })
	httpSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	go httpSrv.Serve(httpLn)
	t.Cleanup(func() { httpSrv.Close() })
	waitTCP(t, httpPort)

	svc, err := service_parser.ParseService(&config.ServiceConfig{
		Name: fmt.Sprintf("mieru-udp-test-%d", mieruPort),
		Addr: fmt.Sprintf(":%d", mieruPort),
		Listener: &config.ListenerConfig{
			Type: "mieru",
			Metadata: map[string]any{
				"users": map[string]any{
					"testuser": "testpass",
				},
				"mtu":      1400,
				"protocol": "udp",
			},
		},
		Handler: &config.HandlerConfig{
			Type: "mieru",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	errc := make(chan error, 1)
	go func() { errc <- svc.Serve() }()
	t.Cleanup(func() { svc.Close() })
	time.Sleep(200 * time.Millisecond)

	cfgPath := filepath.Join(t.TempDir(), "client-udp.json")
	cfg := map[string]any{
		"profiles": []any{
			map[string]any{
				"profileName": "default",
				"user": map[string]any{
					"name":     "testuser",
					"password": "testpass",
				},
				"servers": []any{
					map[string]any{
						"ipAddress": "127.0.0.1",
						"portBindings": []any{
							map[string]any{
								"port":     mieruPort,
								"protocol": "UDP",
							},
						},
					},
				},
				"mtu": 1400,
				"multiplexing": map[string]any{
					"level": "MULTIPLEXING_LOW",
				},
			},
		},
		"activeProfile": "default",
		"rpcPort":       rpcPort,
		"socks5Port":    socksPort,
		"loggingLevel":  "ERROR",
		"advancedSettings": map[string]any{
			"noCheckUpdate": true,
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	runMieru(t, mieruBin, "stop")
	t.Cleanup(func() { runMieru(t, mieruBin, "stop") })

	if out, err := exec.Command(mieruBin, "apply", "config", cfgPath).CombinedOutput(); err != nil {
		t.Fatalf("mieru apply config: %v\n%s", err, out)
	}
	startCmd := exec.Command(mieruBin, "start")
	startCmd.Stdout = os.Stdout
	startCmd.Stderr = os.Stderr
	if err := startCmd.Run(); err != nil {
		t.Fatalf("mieru start: %v", err)
	}
	waitTCP(t, socksPort)

	testURL := fmt.Sprintf("http://127.0.0.1:%d/", httpPort)
	if out, err := exec.Command(mieruBin, "test", testURL).CombinedOutput(); err != nil {
		t.Fatalf("mieru test %s: %v\n%s", testURL, err, out)
	}
	t.Logf("mieru UDP transport test succeeded via socks5://127.0.0.1:%d -> %s", socksPort, testURL)
}

func runMieru(t *testing.T, bin string, args ...string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil && len(args) == 1 && args[0] == "stop" {
		return
	}
	if err != nil {
		t.Logf("mieru %v: %v (%s)", args, err, out)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	return port
}

func waitTCP(t *testing.T, port int) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", addr)
}

func isClosed(err error) bool {
	if err == nil {
		return false
	}
	return err.Error() == "use of closed network connection" ||
		err.Error() == "net.ErrClosed" ||
		contains(err.Error(), "closed")
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
