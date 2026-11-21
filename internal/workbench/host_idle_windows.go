//go:build windows

package workbench

import (
	"context"
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetLastInputInfo = windows.NewLazySystemDLL("user32.dll").NewProc("GetLastInputInfo")
	procGetTickCount     = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount")
)

type lastInputInfo struct {
	size uint32
	time uint32
}

// hostInputIdle is the time since the last input in this process's session.
// A process outside the interactive session cannot read it and reports an
// error, which the update decision treats as unknown.
func hostInputIdle(context.Context) (time.Duration, error) {
	info := lastInputInfo{size: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if ok, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		return 0, errors.New("GetLastInputInfo failed")
	}
	now, _, _ := procGetTickCount.Call()
	return time.Duration(uint32(now)-info.time) * time.Millisecond, nil
}
