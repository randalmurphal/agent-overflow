//go:build windows

package nativenetwork

import (
	"net"

	"golang.org/x/sys/windows"
)

func physicalInterface(iface net.Interface) bool {
	row := windows.MibIfRow2{InterfaceIndex: uint32(iface.Index)}
	if windows.GetIfEntry2Ex(0, &row) != nil {
		return false
	}
	// MIB_IF_ROW2.InterfaceAndOperStatusFlags bit 0 is HardwareInterface.
	return row.InterfaceAndOperStatusFlags&1 != 0
}
