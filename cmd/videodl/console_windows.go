//go:build windows

package main

import "syscall"

// Windows 控制台默认代码页是 GBK(936)，而 Go 一律写 UTF-8 字节流，
// 不切换代码页中文会显示成乱码。这里把控制台输入/输出都切到 UTF-8。
var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP       = kernel32.NewProc("SetConsoleCP")
)

const utf8CodePage = 65001

func enableUTF8Console() {
	_, _, _ = procSetConsoleOutputCP.Call(uintptr(utf8CodePage))
	_, _, _ = procSetConsoleCP.Call(uintptr(utf8CodePage))
}
