//go:build !windows

package main

// 非 Windows 平台控制台本身就是 UTF-8，无需处理。
func enableUTF8Console() {}
