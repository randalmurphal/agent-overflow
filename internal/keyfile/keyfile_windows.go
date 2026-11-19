//go:build windows

package keyfile

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows guards the file with its profile's ACL rather than a mode, so
// this refuses only what would make AO read something other than the file
// itself. The folder is opened once, without following a reparse point (a
// symlink or junction), and checked by its handle; the file is then opened
// relative to that handle (NtCreateFile with RootDirectory), also without
// following a reparse point, and checked by its own handle. Replacing the
// folder after its check cannot redirect the file open.

const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

func (f File) openOwnerOnly() (file *os.File, err error) {
	folder, err := f.openFolder()
	if err != nil {
		return nil, err
	}
	defer func() {
		f.closeInto(&err, closeHandle(folder), "folder")
		if err != nil && file != nil {
			f.closeInto(&err, file.Close, "file")
			file = nil
		}
	}()
	return f.openIn(folder, windows.FILE_GENERIC_READ, windows.FILE_OPEN)
}

func (f File) createOwnerOnly() (created bool, err error) {
	folder, err := f.openFolder()
	if err != nil {
		return false, err
	}
	defer f.closeInto(&err, closeHandle(folder), "folder")
	file, err := f.openIn(folder, windows.FILE_GENERIC_WRITE, windows.FILE_CREATE)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		existing, err := f.openIn(folder, windows.FILE_GENERIC_READ, windows.FILE_OPEN)
		if err != nil {
			return false, err
		}
		return false, existing.Close()
	}
	if err != nil {
		return false, err
	}
	return true, file.Close()
}

// fileDispositionInfo is FILE_DISPOSITION_INFO: its one BOOLEAN marks the
// open file for deletion when its last handle closes.
type fileDispositionInfo struct {
	deleteFile byte
}

// fileAttributeTagInfo is FILE_ATTRIBUTE_TAG_INFO.
type fileAttributeTagInfo struct {
	fileAttributes uint32
	reparseTag     uint32
}

// isLink reports a symbolic link or a junction. Other reparse points (a
// cloud placeholder, a deduplicated file) are the object itself, not a name
// for another one.
func (t fileAttributeTagInfo) isLink() bool {
	return t.fileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 &&
		(t.reparseTag == windows.IO_REPARSE_TAG_SYMLINK || t.reparseTag == windows.IO_REPARSE_TAG_MOUNT_POINT)
}

// removeOwnerOnly opens the key file's name relative to the checked folder
// without following a reparse point, inspects that handle and marks the
// same handle for deletion, so what is checked is what is removed. A link
// there, to a file or a folder, is removed itself; a folder that is not a
// link is refused.
func (f File) removeOwnerOnly() (err error) {
	folder, err := f.openFolder()
	if errors.Is(err, ErrMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.closeInto(&err, closeHandle(folder), "folder")
	objectName, err := windows.NewNTUnicodeString(filepath.Base(f.path))
	if err != nil {
		return err
	}
	handle, err := ntCreateIn(folder, objectName, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT)
	switch {
	case errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND), errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND):
		return nil
	case err != nil:
		return f.failed("remove", "file", err)
	}
	// The deletion takes effect when this handle closes.
	defer f.closeInto(&err, closeHandle(handle), "file")
	var tag fileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err != nil {
		return f.failed("inspect", "file", err)
	}
	if tag.fileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 && !tag.isLink() {
		return f.refused("it is not a regular file")
	}
	info := fileDispositionInfo{deleteFile: 1}
	if err := windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return f.failed("remove", "file", err)
	}
	return nil
}

func (f File) openFolder() (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(filepath.Dir(f.path))
	if err != nil {
		return 0, err
	}
	folder, err := windows.CreateFile(name, windows.FILE_GENERIC_READ, shareAll, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return 0, f.label.ErrMissing()
		}
		return 0, f.failed("open", "folder", err)
	}
	attributes, err := handleAttributes(folder)
	switch {
	case err != nil:
		err = f.failed("inspect", "folder", err)
	case attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		err = f.refused("its folder is a symlink")
	case attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		err = f.refused("its folder is not a folder")
	}
	if err != nil {
		f.closeInto(&err, closeHandle(folder), "folder")
		return 0, err
	}
	return folder, nil
}

func closeHandle(handle windows.Handle) func() error {
	return func() error { return windows.CloseHandle(handle) }
}

// ntCreateIn opens objectName relative to the checked folder handle.
func ntCreateIn(folder windows.Handle, objectName *windows.NTUnicodeString, access, disposition, options uint32) (windows.Handle, error) {
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: folder, ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err := windows.NtCreateFile(&handle, access, &attributes, &status, nil, windows.FILE_ATTRIBUTE_NORMAL, shareAll, disposition, options, 0, 0)
	return handle, err
}

// openIn opens the key file relative to the checked folder handle.
func (f File) openIn(folder windows.Handle, access, disposition uint32) (*os.File, error) {
	objectName, err := windows.NewNTUnicodeString(filepath.Base(f.path))
	if err != nil {
		return nil, err
	}
	handle, err := ntCreateIn(folder, objectName, access, disposition,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT)
	switch {
	case errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND), errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND):
		return nil, f.label.ErrMissing()
	case errors.Is(err, windows.STATUS_FILE_IS_A_DIRECTORY):
		return nil, f.refused("it is not a regular file")
	case errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION):
		return nil, err
	case err != nil:
		return nil, f.failed("open", "file", err)
	}
	fileAttributes, err := handleAttributes(handle)
	switch {
	case err != nil:
		err = f.failed("inspect", "file", err)
	case fileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		err = f.refused("it is a symlink")
	}
	if err != nil {
		f.closeInto(&err, closeHandle(handle), "file")
		return nil, err
	}
	return os.NewFile(uintptr(handle), f.path), nil
}

func handleAttributes(handle windows.Handle) (uint32, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return 0, err
	}
	return info.FileAttributes, nil
}
