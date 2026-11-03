package tunnel

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
)

// bufferedConn reads through the reader used to sniff the protocol, so no peeked bytes are lost.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(buffer []byte) (int, error) {
	return c.reader.Read(buffer)
}

// httpProxyHandshake accepts an HTTP proxy request and returns the target and any bytes to send first.
// CONNECT is tunneled as is; a plain request is rewritten to origin form and sent with Connection: close.
func httpProxyHandshake(conn net.Conn, reader *bufio.Reader) (string, []byte, error) {
	request, err := http.ReadRequest(reader)
	if err != nil {
		return "", nil, err
	}
	if request.Method == http.MethodConnect {
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return "", nil, err
		}
		return request.URL.Host, nil, nil
	}
	if request.URL.Host == "" {
		io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n")
		return "", nil, errors.New("not a proxy request")
	}
	target := request.URL.Host
	if request.URL.Port() == "" {
		target = net.JoinHostPort(request.URL.Hostname(), "80")
	}
	request.Header.Del("Proxy-Connection")
	request.Header.Del("Proxy-Authorization")
	request.Close = true
	var head bytes.Buffer
	if err := request.Write(&head); err != nil {
		return "", nil, err
	}
	return target, head.Bytes(), nil
}
