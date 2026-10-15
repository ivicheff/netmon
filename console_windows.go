//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Создаёт консольное окно для процесса, собранного как windowsgui.
func attachConsole() {
	k := syscall.NewLazyDLL("kernel32.dll")
	k.NewProc("AllocConsole").Call()
	k.NewProc("SetConsoleOutputCP").Call(65001)
	if t, err := syscall.UTF16PtrFromString("Netmon"); err == nil {
		k.NewProc("SetConsoleTitleW").Call(uintptr(unsafe.Pointer(t)))
	}
	f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		return
	}
	os.Stdout, os.Stderr = f, f
	var mode uint32
	h := syscall.Handle(f.Fd())
	if syscall.GetConsoleMode(h, &mode) == nil {
		k.NewProc("SetConsoleMode").Call(uintptr(h), uintptr(mode|0x0004)) // цвета ANSI
	}
}
