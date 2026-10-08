package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyLoadedBeforeFirstRequest(t *testing.T) {
	if path := os.Getenv("XRW_PROXY_TEST_CONFIG"); path != "" {
		if err := loadEnvFile(path); err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequest(http.MethodGet, "https://api.telegram.org", nil)
		proxy, err := http.ProxyFromEnvironment(request)
		if err != nil || proxy == nil || proxy.String() != "http://127.0.0.1:10808" {
			t.Fatal("Telegram did not use HTTPS_PROXY from the private environment file")
		}
		return
	}
	path := filepath.Join(t.TempDir(), "local-uploader.env")
	if err := os.WriteFile(path, []byte("HTTPS_PROXY=http://127.0.0.1:10808\nNO_PROXY=localhost,127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Go caches proxy environment variables. Exercise first-request startup in
	// a clean child process so test order cannot hide a regression.
	command := exec.Command(os.Args[0], "-test.run=^TestProxyLoadedBeforeFirstRequest$")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "XRW_PROXY_TEST_CONFIG":
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "XRW_PROXY_TEST_CONFIG="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("proxy startup check failed: %v\n%s", err, output)
	}
}
