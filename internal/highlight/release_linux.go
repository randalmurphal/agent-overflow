package highlight

/*
#include <malloc.h>
*/
import "C"

// releaseFreedMemory returns free pages in every glibc arena to the OS,
// including pages between live blocks that ordinary top-of-heap trimming
// keeps.
func releaseFreedMemory() {
	C.malloc_trim(0)
}
