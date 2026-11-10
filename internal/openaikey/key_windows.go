//go:build windows

package openaikey

import (
	"errors"
	"fmt"
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

func openOwnerOnly(path string) (file *os.File, err error) {
	folder, err := openFolder(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() {
		closeInto(&err, closeHandle(folder), "folder")
		if err != nil && file != nil {
			closeInto(&err, file.Close, "file")
			file = nil
		}
	}()
	return openIn(folder, path, windows.FILE_GENERIC_READ, windows.FILE_OPEN)
}

func createOwnerOnly(path string) (created bool, err error) {
	folder, err := openFolder(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	defer closeInto(&err, closeHandle(folder), "folder")
	file, err := openIn(folder, path, windows.FILE_GENERIC_WRITE, windows.FILE_CREATE)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		existing, err := openIn(folder, path, windows.FILE_GENERIC_READ, windows.FILE_OPEN)
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

func openFolder(dir string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	folder, err := windows.CreateFile(name, windows.FILE_GENERIC_READ, shareAll, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return 0, ErrMissing
		}
		return 0, fmt.Errorf("open the OpenAI API key folder: %w", err)
	}
	attributes, err := handleAttributes(folder)
	switch {
	case err != nil:
		err = fmt.Errorf("inspect the OpenAI API key folder: %w", err)
	case attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		err = refused("its folder is a symlink")
	case attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		err = refused("its folder is not a folder")
	}
	if err != nil {
		closeInto(&err, closeHandle(folder), "folder")
		return 0, err
	}
	return folder, nil
}

func closeHandle(handle windows.Handle) func() error {
	return func() error { return windows.CloseHandle(handle) }
}

// openIn opens path's file relative to the checked folder handle.
func openIn(folder windows.Handle, path string, access, disposition uint32) (*os.File, error) {
	objectName, err := windows.NewNTUnicodeString(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: folder, ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, access, &attributes, &status, nil, windows.FILE_ATTRIBUTE_NORMAL, shareAll, disposition,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	switch {
	case errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND), errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND):
		return nil, ErrMissing
	case errors.Is(err, windows.STATUS_FILE_IS_A_DIRECTORY):
		return nil, refused("it is not a regular file")
	case errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("open the OpenAI API key file: %w", err)
	}
	fileAttributes, err := handleAttributes(handle)
	switch {
	case err != nil:
		err = fmt.Errorf("inspect the OpenAI API key file: %w", err)
	case fileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		err = refused("it is a symlink")
	}
	if err != nil {
		closeInto(&err, closeHandle(handle), "file")
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func handleAttributes(handle windows.Handle) (uint32, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return 0, err
	}
	return info.FileAttributes, nil
}
