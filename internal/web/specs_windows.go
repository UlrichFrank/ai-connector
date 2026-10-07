package web

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// probe asks Windows (registry, GlobalMemoryStatusEx, RtlGetVersion).
func probe() Specs {
	sp := Specs{OS: "Windows"}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		sp.Chip, _, _ = k.GetStringValue("ProcessorNameString")
		k.Close()
	}
	ms := memoryStatusEx{}
	ms.length = uint32(unsafe.Sizeof(ms))
	if ok, _, _ := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); ok != 0 {
		sp.RAMGB = gb(int64(ms.totalPhys))
	}
	v := windows.RtlGetVersion()
	sp.OSVersion = fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	return sp
}

var globalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// memoryStatusEx is Win32's MEMORYSTATUSEX.
type memoryStatusEx struct {
	length, memoryLoad                    uint32
	totalPhys, availPhys                  uint64
	totalPageFile, availPageFile          uint64
	totalVirtual, availVirtual, availExtV uint64
}
