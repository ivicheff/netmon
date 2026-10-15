//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

var (
	iphlpapi = syscall.NewLazyDLL("iphlpapi.dll")
	pTcp     = iphlpapi.NewProc("GetExtendedTcpTable")
	pUdp     = iphlpapi.NewProc("GetExtendedUdpTable")
)

func getTable(proc *syscall.LazyProc, af, class uintptr) ([]byte, error) {
	var size uint32
	var buf []byte
	for i := 0; i < 6; i++ {
		var p uintptr
		if len(buf) > 0 {
			p = uintptr(unsafe.Pointer(&buf[0]))
		}
		r, _, _ := proc.Call(p, uintptr(unsafe.Pointer(&size)), 0, af, class, 0)
		switch r {
		case 0:
			return buf, nil
		case 122: // ERROR_INSUFFICIENT_BUFFER
			buf = make([]byte, size+8192)
		default:
			return nil, fmt.Errorf("код ошибки Windows %d", r)
		}
	}
	return nil, fmt.Errorf("таблица постоянно меняется")
}

func port(v uint32) uint16 { return uint16(v&0xff)<<8 | uint16(v>>8&0xff) }

func procNames() map[uint32]string {
	m := map[uint32]string{}
	h, err := syscall.CreateToolhelp32Snapshot(2, 0) // TH32CS_SNAPPROCESS
	if err != nil {
		return m
	}
	defer syscall.CloseHandle(h)
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if syscall.Process32First(h, &e) != nil {
		return m
	}
	for {
		m[e.ProcessID] = syscall.UTF16ToString(e.ExeFile[:])
		if syscall.Process32Next(h, &e) != nil {
			break
		}
	}
	return m
}

func collect() ([]tcpConn, []uint32, map[uint32]string, error) {
	le := binary.LittleEndian
	var tcp []tcpConn
	var udp []uint32

	// TCP IPv4
	if b, err := getTable(pTcp, 2, 5); err != nil {
		return nil, nil, nil, err
	} else if len(b) >= 4 {
		n := int(le.Uint32(b))
		for i := 0; i < n && 4+(i+1)*24 <= len(b); i++ {
			r := b[4+i*24:]
			tcp = append(tcp, tcpConn{
				pid:         le.Uint32(r[20:]),
				remote:      net.IP(append([]byte(nil), r[12:16]...)),
				rport:       port(le.Uint32(r[16:])),
				established: le.Uint32(r[0:]) == 5,
			})
		}
	}
	// TCP IPv6
	if b, err := getTable(pTcp, 23, 5); err == nil && len(b) >= 4 {
		n := int(le.Uint32(b))
		for i := 0; i < n && 4+(i+1)*56 <= len(b); i++ {
			r := b[4+i*56:]
			tcp = append(tcp, tcpConn{
				pid:         le.Uint32(r[52:]),
				remote:      net.IP(append([]byte(nil), r[24:40]...)),
				rport:       port(le.Uint32(r[44:])),
				established: le.Uint32(r[48:]) == 5,
			})
		}
	}
	// UDP IPv4
	if b, err := getTable(pUdp, 2, 1); err == nil && len(b) >= 4 {
		n := int(le.Uint32(b))
		for i := 0; i < n && 4+(i+1)*12 <= len(b); i++ {
			udp = append(udp, le.Uint32(b[4+i*12+8:]))
		}
	}
	// UDP IPv6
	if b, err := getTable(pUdp, 23, 1); err == nil && len(b) >= 4 {
		n := int(le.Uint32(b))
		for i := 0; i < n && 4+(i+1)*28 <= len(b); i++ {
			udp = append(udp, le.Uint32(b[4+i*28+24:]))
		}
	}
	return tcp, udp, procNames(), nil
}
