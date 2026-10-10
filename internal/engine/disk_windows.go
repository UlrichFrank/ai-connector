package engine

import "golang.org/x/sys/windows"

// freeBytes is the free space for this user on the volume of dir (an existing folder).
func freeBytes(dir string) int64 {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0
	}
	var free, total, all uint64
	if windows.GetDiskFreeSpaceEx(p, &free, &total, &all) != nil {
		return 0
	}
	return int64(free)
}
