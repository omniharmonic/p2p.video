package httpserver

import (
	"io"
	"net/http"
	"strings"
)

// adminPost POSTs JSON to a management route as the local host app would:
// direct loopback plus the desktop token (servers under test use
// testDesktopToken) [SL-SEC-1].
func adminPost(url, body string) (*http.Response, error) {
	return adminDo(http.MethodPost, url, body)
}

// adminGet GETs a management route with the desktop token.
func adminGet(url string) (*http.Response, error) {
	return adminDo(http.MethodGet, url, "")
}

func adminDo(method, url, body string) (*http.Response, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testDesktopToken)
	return http.DefaultClient.Do(req)
}
