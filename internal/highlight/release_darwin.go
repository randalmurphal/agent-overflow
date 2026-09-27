package highlight

/*
#include <malloc/malloc.h>
*/
import "C"

// releaseFreedMemory returns free pages in every malloc zone to the OS.
func releaseFreedMemory() {
	C.malloc_zone_pressure_relief(nil, 0)
}
