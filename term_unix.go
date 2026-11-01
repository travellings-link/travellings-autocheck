//go:build !windows

package main

import "os"

// enableVT 在 Unix 上只需判断是否为字符设备；ANSII 由终端自身解析。
func enableVT(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
