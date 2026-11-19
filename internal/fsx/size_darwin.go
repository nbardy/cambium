//go:build darwin

package fsx

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

type attrList struct {
	bitmapcount uint16
	reserved    uint16
	commonattr  uint32
	volattr     uint32
	dirattr     uint32
	fileattr    uint32
	forkattr    uint32
}

const (
	attrBitMapCount       = 5
	attrCmnExtPrivateSize = 0x8 // carried in forkattr under FSOPT_ATTR_CMN_EXTENDED
	fsoptNoFollow         = 0x1
	fsoptAttrCmnExtended  = 0x20
)

// privateSize returns bytes of path's data not shared with any other clone.
func privateSize(path string) (int64, bool) {
	name, err := syscall.BytePtrFromString(path)
	if err != nil {
		return 0, false
	}
	list := attrList{bitmapcount: attrBitMapCount, forkattr: attrCmnExtPrivateSize}
	var buf [16]byte
	_, _, errno := syscall.Syscall6(syscall.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&list)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		fsoptNoFollow|fsoptAttrCmnExtended, 0)
	if errno != 0 || binary.LittleEndian.Uint32(buf[:4]) < 12 {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(buf[4:12])), true
}
