package tailscale

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverPrivateMatchingServe(t *testing.T) {
	cases := []struct{ json, want string }{
		{`{"Web":{"host.tail123.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7443"}}}}}`, "https://host.tail123.ts.net"},
		{`{"Web":{"host.tail123.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://localhost:7443/"}}}}}`, "https://host.tail123.ts.net:8443"},
		{`{"Web":{"host.tail123.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8789"}}}}}`, ""},
		{`{"Web":{"host.tail123.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7443"}}}},"AllowFunnel":{"host.tail123.ts.net:443":true}}`, ""},
		{`{"Web":{"host.tail123.ts.net:443":{"Handlers":{"/":{"Proxy":"http://10.0.0.1:7443"}}}}}`, ""},
		{`{}`, ""}, {`invalid`, ""},
	}
	for _, c := range cases {
		got, err := guestURLFromServe([]byte(c.json), 7443)
		if got != c.want || (c.want == "") != (err != nil) {
			t.Fatalf("got %q err %v; want %q", got, err, c.want)
		}
	}
}

func TestMacApplicationCLIWithoutShellAlias(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "Tailscale")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	noPATH := func(string) (string, error) { return "", fmt.Errorf("absent") }
	home := func() (string, error) { return "/fictional-home", nil }
	systemPath := "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
	stat := func(path string) (os.FileInfo, error) {
		if path == systemPath {
			return os.Stat(real)
		}
		return nil, os.ErrNotExist
	}
	got, err := findExecutable(noPATH, stat, "darwin", home)
	if err != nil || got != systemPath {
		t.Fatalf("app CLI: %q %v", got, err)
	}
	if got, err = findExecutable(noPATH, stat, "linux", home); err == nil || got != "" {
		t.Fatal("must not probe Mac applications on Linux")
	}
	if err = os.Chmod(real, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = findExecutable(noPATH, stat, "darwin", home); err == nil {
		t.Fatal("non executable must be rejected")
	}
	pathCLI := func(string) (string, error) { return "/chosen/tailscale", nil }
	if got, err = findExecutable(pathCLI, stat, "darwin", home); err != nil || got != "/chosen/tailscale" {
		t.Fatal("installed CLI preference lost")
	}
}
