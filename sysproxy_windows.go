//go:build windows

package main

import "golang.org/x/sys/windows/registry"

// Читает системный прокси Windows (Параметры → Сеть → Прокси).
func systemProxy() string {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	en, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || en == 0 {
		return ""
	}
	s, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return ""
	}
	return parseProxy(s)
}
