//go:build !windows

package main

import "errors"

func collect() ([]tcpConn, []uint32, map[uint32]string, error) {
	return nil, nil, nil, errors.New("поддерживается только Windows")
}
