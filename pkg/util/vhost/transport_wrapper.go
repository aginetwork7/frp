package vhost

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

var _ http.RoundTripper = (*transportWrapper)(nil)

type transportWrapper struct {
	transport http.RoundTripper
}

func (t transportWrapper) RoundTrip(request *http.Request) (*http.Response, error) {
	rsp, err := t.transport.RoundTrip(request)
	if err != nil {
		return nil, err
	}

	fmt.Printf("%#v\n", request.Header)
	if h := rsp.Header.Get("Content-Type"); strings.Contains(h, "text/html") {
		data, err := io.ReadAll(rsp.Body)
		if err != nil {
			return nil, err
		}

		dataList := strings.SplitN(string(data), "</head>", 2)
		body := dataList[0]
		body += `<meta http-equiv="Content-Security-Policy" content="upgrade-insecure-requests"/>`
		body += `</head>`
		body += dataList[1]
		rsp.Body = io.NopCloser(strings.NewReader(body))
	}

	return rsp, err
}
