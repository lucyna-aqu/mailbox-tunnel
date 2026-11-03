//go:build !windows

package main

import "errors"

var errNoSystemProxy = errors.New("setting the system proxy is only supported on Windows")

func readSystemProxy() (ProxySettings, error) {
	return ProxySettings{}, errNoSystemProxy
}

func writeSystemProxy(ProxySettings) error {
	return errNoSystemProxy
}
