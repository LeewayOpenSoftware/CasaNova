package file

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"os"
	"path"
	path2 "path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mholt/archiver/v3"
)

// GetSize get the file size
func GetSize(f multipart.File) (int, error) {
	content, err := io.ReadAll(f)
	return len(content), err
}

// GetExt get the file ext
func GetExt(fileName string) string {
	return path.Ext(fileName)
}

// CheckNotExist check if the file exists
func CheckNotExist(src string) bool {
	_, err := os.Stat(src)

	return os.IsNotExist(err)
}

// CheckPermission check if the file has permission
func CheckPermission(src string) bool {
	_, err := os.Stat(src)

	return os.IsPermission(err)
}

// IsNotExistMkDir create a directory if it does not exist
func IsNotExistMkDir(src string) error {
	if notExist := CheckNotExist(src); notExist {
		if err := MkDir(src); err != nil {
			return err
		}
	}

	return nil
}

// MkDir create a directory
func MkDir(src string) error {
	err := os.MkdirAll(src, os.ModePerm)
	if err != nil {
		return err
	}
	os.Chmod(src, 0o777)

	return nil
}

// RMDir remove a directory
func RMDir(src string) error {
	err := os.RemoveAll(src)
	if err != nil {
		return err
	}
	os.Remove(src)
	return nil
}

func RemoveAll(dir string) error {
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return os.Remove(path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.Remove(dir)
}

// Open a file according to a specific mode
func Open(name string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}

	return f, nil
}

// MustOpen maximize trying to open the file
func MustOpen(fileName, filePath string) (*os.File, error) {
	//dir, err := os.Getwd()
	//if err != nil {
	//	return nil, fmt.Errorf("os.Getwd err: %v", err)
	//}

	src := filePath
	perm := CheckPermission(src)
	if perm == true {
		return nil, fmt.Errorf("file.CheckPermission Permission denied src: %s", src)
	}

	err := IsNotExistMkDir(src)
	if err != nil {
		return nil, fmt.Errorf("file.IsNotExistMkDir src: %s, err: %v", src, err)
	}

	f, err := Open(src+fileName, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("Fail to OpenFile :%v", err)
	}

	return f, nil
}

// Exists reports whether path exists. Wave 33-A HIGH: previously checked
// `os.IsExist(err)` which is the wrong sentinel — that only matches the
// EEXIST that O_CREATE|O_EXCL produces on collisions, not the ENOENT
// that `os.Stat` returns when a path is missing. As a result every error
// path (including EACCES on a real, existing file) reported "missing",
// and callers like the file-action handlers happily skipped paths that
// actually existed but were just not readable to the daemon's UID.
func Exists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	return !os.IsNotExist(err)
}

func IsDir(path string) bool {
	s, err := os.Stat(path)
	if err != nil {
		return false
	}
	return s.IsDir()
}

func IsFile(path string) bool {
	return !IsDir(path)
}

func CreateFile(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return nil
}

func CreateFileAndWriteContent(path string, content string) error {
	tmpPath := path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}

	write := bufio.NewWriter(file)
	if _, err := write.WriteString(content); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := write.Flush(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// IsNotExistCreateFile create a file if it does not exist
func IsNotExistCreateFile(src string) error {
	if notExist := CheckNotExist(src); notExist {
		if err := CreateFile(src); err != nil {
			return err
		}
	}

	return nil
}

func ReadFullFile(path string) []byte {
	file, err := os.Open(path)
	if err != nil {
		return []byte("")
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return []byte("")
	}
	return content
}

// File copies a single file from src to dst using a tmp+rename pattern.
// A crash mid-copy leaves a .casanova-tmp stub (swept on startup) but
// NEVER a half-written file at the destination.
func CopyFile(src, dst, style string) error {
	lastPath := src[strings.LastIndex(src, "/")+1:]

	if !strings.HasSuffix(dst, "/") {
		dst += "/"
	}
	dst += lastPath
	if Exists(dst) {
		if style == "skip" {
			return nil
		}
		if err := os.Remove(dst); err != nil {
			return err
		}
	}

	srcinfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	return copyFileAtomic(src, dst, srcinfo.Mode())
}

/**
 * @description:
 * @param {*} src
 * @param {*} dst
 * @param {string} style
 * @return {*}
 * @method:
 * @router:
 */
func CopySingleFile(src, dst, style string) error {
	if Exists(dst) {
		if style == "skip" {
			return nil
		}
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	srcinfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	return copyFileAtomic(src, dst, srcinfo.Mode())
}

// Check for duplicate file names
func GetNoDuplicateFileName(fullPath string) string {
	path, fileName := filepath.Split(fullPath)
	fileSuffix := path2.Ext(fileName)
	filenameOnly := strings.TrimSuffix(fileName, fileSuffix)
	for i := 0; Exists(fullPath); i++ {
		fullPath = path2.Join(path, filenameOnly+"("+strconv.Itoa(i+1)+")"+fileSuffix)
	}
	return fullPath
}

// Dir copies a whole directory recursively
func CopyDir(src string, dst string, style string) error {
	var err error
	var fds []os.DirEntry
	var srcinfo os.FileInfo

	if srcinfo, err = os.Stat(src); err != nil {
		return err
	}
	if !srcinfo.IsDir() {
		if err = CopyFile(src, dst, style); err != nil {
			fmt.Println(err)
		}
		return nil
	}
	// dstPath := dst
	lastPath := src[strings.LastIndex(src, "/")+1:]
	dst += "/" + lastPath
	// for i := 0; Exists(dst); i++ {
	// 	dst = dstPath + "/" + lastPath + strconv.Itoa(i+1)
	// }
	if Exists(dst) {
		if style == "skip" {
			return nil
		} else {
			os.Remove(dst)
		}
	}
	if err = os.MkdirAll(dst, srcinfo.Mode()); err != nil {
		return err
	}
	if fds, err = os.ReadDir(src); err != nil {
		return err
	}
	for _, fd := range fds {
		srcfp := path.Join(src, fd.Name())
		dstfp := dst // path.Join(dst, fd.Name())

		if fd.IsDir() {
			if err = CopyDir(srcfp, dstfp, style); err != nil {
				fmt.Println(err)
			}
		} else {
			if err = CopyFile(srcfp, dstfp, style); err != nil {
				fmt.Println(err)
			}
		}
	}
	return nil
}

func WriteToPath(data []byte, path, name string) error {
	fullPath := path
	if strings.HasSuffix(path, "/") {
		fullPath += name
	} else {
		fullPath += "/" + name
	}
	return WriteToFullPath(data, fullPath, 0o666)
}

// WriteToFullPath writes `data` to `fullPath` atomically (write-temp +
// fsync + rename). Wave 33-A MEDIUM: callers historically passed 0o666
// here for state files like baseinfo.conf, exposing the device
// fingerprint (and other persisted secrets like ZeroTier auth tokens)
// world-readable on multi-user CasaNova boxes. The function now masks
// off any group/other write bits (`&^ 0o022`) so a buggy caller cannot
// accidentally widen permissions, regardless of process umask. Read
// bits for group/other are preserved when the caller asked for them —
// callers that need 0o600 must request it explicitly.
// WriteToFullPath writes data and publishes it with rename(2). The temp file
// has a RANDOM name (CreateTemp), not the old fixed fullPath+".tmp":
//
//   - the fixed name was pre-plantable. A symlink dropped at "<dest>.tmp" was
//     opened WITHOUT O_EXCL and followed, so an attacker who could guess a
//     path CasaNova would write (PutFileContent's directories are
//     user-controlled) redirected the write anywhere on the host, as root.
//   - two concurrent writers to the same fullPath shared that one tmp name
//     and clobbered each other's bytes, publishing a torn file.
//
// The destination itself is still replaced by rename, exactly as before — a
// symlink AT the destination is replaced (not written through), which is the
// long-standing behaviour every caller already expects.
func WriteToFullPath(data []byte, fullPath string, perm fs.FileMode) error {
	perm = perm &^ 0o022
	dir := filepath.Dir(fullPath)
	if err := IsNotExistMkDir(dir); err != nil {
		return err
	}

	// Temp in the SAME directory so the rename stays same-filesystem atomic
	// (a temp in /tmp would make rename a cross-device copy, defeating the
	// point). Random name closes the plant-and-clobber holes above.
	file, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	tmpPath := file.Name()

	// CreateTemp forces 0600; restore the caller's intended mode. PutFileContent
	// passes the existing file's mode (setuid/setgid bits included), and the
	// old code preserved it via the OpenFile perm arg — so this must too, or an
	// executable/sgid file silently loses its bits on every edit.
	if err := file.Chmod(perm); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}

	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}

	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}

	if err := file.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, fullPath); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return nil
}

// SpliceFiles concatenates chunk files produced by a chunked upload into a
// single destination file. The chunks are removed only after the assembled
// file has been synced and renamed into place — they are the upload's resume
// state, and deleting them any earlier means one crash mid-splice loses both
// the chunks and the partial result (see the comment at the deletion site).
func SpliceFiles(dir, path string, length int, startPoint int) error {
	fullPath := path
	tmpPath := fullPath + ".tmp"

	if err := IsNotExistCreateFile(tmpPath); err != nil {
		return err
	}

	file, err := os.OpenFile(tmpPath,
		os.O_WRONLY|os.O_TRUNC|os.O_CREATE,
		0o666,
	)
	if err != nil {
		return err
	}
	defer file.Close()

	bufferedWriter := bufio.NewWriter(file)

	// Chunks are deleted only AFTER the assembled file has been synced and
	// renamed into place. The previous shape deleted each chunk the moment
	// it was consumed — to avoid holding 2x the size on disk — which meant a
	// crash, a full disk at Flush, or any late error destroyed both sides at
	// once: the chunks were gone and the error path removed the partial tmp
	// too. An interrupted upload the user can RESUME (the chunks are the
	// resume state) became an upload they must redo from byte zero. The 2x
	// window is the price of never losing both copies; it lasts only for the
	// duration of the splice.
	chunkPaths := make([]string, 0, length)
	for i := 0; i < length+startPoint-1; i++ {
		chunkPath := dir + "/" + strconv.Itoa(i+startPoint)
		data, err := os.ReadFile(chunkPath)
		if err != nil {
			os.Remove(tmpPath)
			return err
		}
		if _, err := bufferedWriter.Write(data); err != nil {
			os.Remove(tmpPath)
			return err
		}
		chunkPaths = append(chunkPaths, chunkPath)
	}

	if err := bufferedWriter.Flush(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := file.Sync(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	// Close before rename so the fd is released on all platforms.
	file.Close()
	if err := os.Rename(tmpPath, fullPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	// The assembled file is in place; the chunks are now redundant.
	for _, chunkPath := range chunkPaths {
		if err := os.Remove(chunkPath); err != nil {
			// Non-fatal: worst case is leftover chunks the startup sweep
			// cleans; the user's data is already safe.
			log.Printf("SpliceFiles: failed to remove %s: %v", chunkPath, err)
		}
	}
	return nil
}

func GetCompressionAlgorithm(t string) (string, archiver.Writer, error) {
	switch t {
	case "zip", "":
		return ".zip", archiver.NewZip(), nil
	case "tar":
		return ".tar", archiver.NewTar(), nil
	case "targz":
		return ".tar.gz", archiver.NewTarGz(), nil
	case "tarbz2":
		return ".tar.bz2", archiver.NewTarBz2(), nil
	case "tarxz":
		return ".tar.xz", archiver.NewTarXz(), nil
	case "tarlz4":
		return ".tar.lz4", archiver.NewTarLz4(), nil
	case "tarsz":
		return ".tar.sz", archiver.NewTarSz(), nil
	default:
		return "", nil, errors.New("format not implemented")
	}
}

func AddFile(ar archiver.Writer, path, commonPath string) error {
	// Lstat (not Stat) so we can detect symlinks before following them.
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	// Symlinks: resolve the real target and verify it stays within the
	// archive's base directory (commonPath). Without this check a symlink
	// pointing to e.g. /etc/passwd would leak sensitive files into the
	// downloaded archive.
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, terr := filepath.EvalSymlinks(path)
		if terr != nil {
			log.Printf("AddFile: skipping broken symlink %s: %v", path, terr)
			return nil
		}
		// Ensure the resolved target is under the base directory.
		absCommon, _ := filepath.Abs(commonPath)
		absResolved, _ := filepath.Abs(resolved)
		if !strings.HasPrefix(absResolved, absCommon+string(filepath.Separator)) && absResolved != absCommon {
			log.Printf("AddFile: skipping symlink %s -> %s (escapes base directory %s)", path, resolved, commonPath)
			return nil
		}
		target, terr := os.Stat(path) // follows the link
		if terr != nil {
			log.Printf("AddFile: skipping unresolvable symlink %s: %v", path, terr)
			return nil
		}
		info = target
	}

	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	if path != commonPath {
		//filename := info.Name()
		filename := strings.TrimPrefix(path, commonPath)
		filename = strings.TrimPrefix(filename, string(filepath.Separator))
		err = ar.Write(archiver.File{
			FileInfo: archiver.FileInfo{
				FileInfo:   info,
				CustomName: filename,
			},
			ReadCloser: file,
		})
		if err != nil {
			return err
		}
	}

	if info.IsDir() {
		names, err := file.Readdirnames(0)
		if err != nil {
			return err
		}

		for _, name := range names {
			err = AddFile(ar, filepath.Join(path, name), commonPath)
			if err != nil {
				log.Printf("Failed to archive %v", err)
			}
		}
	}

	return nil
}

func CommonPrefix(sep byte, paths ...string) string {
	// Handle special cases.
	switch len(paths) {
	case 0:
		return ""
	case 1:
		return path.Clean(paths[0])
	}

	// Note, we treat string as []byte, not []rune as is often
	// done in Go. (And sep as byte, not rune). This is because
	// most/all supported OS' treat paths as string of non-zero
	// bytes. A filename may be displayed as a sequence of Unicode
	// runes (typically encoded as UTF-8) but paths are
	// not required to be valid UTF-8 or in any normalized form
	// (e.g. "é" (U+00C9) and "é" (U+0065,U+0301) are different
	// file names.
	c := []byte(path.Clean(paths[0]))

	// We add a trailing sep to handle the case where the
	// common prefix directory is included in the path list
	// (e.g. /home/user1, /home/user1/foo, /home/user1/bar).
	// path.Clean will have cleaned off trailing / separators with
	// the exception of the root directory, "/" (in which case we
	// make it "//", but this will get fixed up to "/" bellow).
	c = append(c, sep)

	// Ignore the first path since it's already in c
	for _, v := range paths[1:] {
		// Clean up each path before testing it
		v = path.Clean(v) + string(sep)

		// Find the first non-common byte and truncate c
		if len(v) < len(c) {
			c = c[:len(v)]
		}
		for i := 0; i < len(c); i++ {
			if v[i] != c[i] {
				c = c[:i]
				break
			}
		}
	}

	// Remove trailing non-separator characters and the final separator
	for i := len(c) - 1; i >= 0; i-- {
		if c[i] == sep {
			c = c[:i]
			break
		}
	}

	return string(c)
}

func GetFileOrDirSize(path string) (int64, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if fileInfo.IsDir() {
		return DirSizeB(path + "/")
	}
	return fileInfo.Size(), nil
}

// getFileSize get file size by path(B)
func DirSizeB(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		// Walk calls back with err != nil and info == nil for entries it
		// could not lstat — a file deleted mid-walk, a permission-denied
		// subtree, a broken mount. Dereferencing info there panics, and the
		// panic happens inside whatever goroutine asked for the size (this
		// is called when queueing copies). Skip those entries: the result is
		// an approximate size, which is what every caller already treats it as.
		if err != nil || info == nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func MoveFile(sourcePath, destPath string) error {
	inputFile, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("Couldn't open source file: %s", err)
	}
	outputFile, err := os.Create(destPath)
	if err != nil {
		inputFile.Close()
		return fmt.Errorf("Couldn't open dest file: %s", err)
	}
	defer outputFile.Close()
	_, err = io.Copy(outputFile, inputFile)
	inputFile.Close()
	if err != nil {
		return fmt.Errorf("Writing to output file failed: %s", err)
	}
	err = os.Remove(sourcePath)
	if err != nil {
		return fmt.Errorf("Failed removing original file: %s", err)
	}
	return nil
}

func ReadLine(lineNumber int, path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	// Close on EVERY exit path. The defer used to sit after the loop, so the
	// common case — finding the line and returning from inside the loop —
	// leaked the descriptor.
	defer file.Close()
	fileScanner := bufio.NewScanner(file)
	lineCount := 1
	for fileScanner.Scan() {
		if lineCount == lineNumber {
			return fileScanner.Text()
		}
		lineCount++
	}
	return ""
}

func NameAccumulation(name string, dir string) string {
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return name
	}
	base := name
	strings.Split(base, "_")
	index := strings.LastIndex(base, "_")
	if index < 0 {
		index = len(base)
	}
	for i := 1; ; i++ {
		newPath := filepath.Join(dir, fmt.Sprintf("%s_%d", base[:index], i))
		if _, err := os.Stat(newPath); os.IsNotExist(err) {
			return fmt.Sprintf("%s_%d", base[:index], i)
		}
	}
}

func ParseFileHeader(h []byte, boundary []byte) (map[string]string, bool) {
	arr := bytes.Split(h, boundary)
	//var out_header FileHeader
	//out_header.ContentLength = -1
	const (
		CONTENT_DISPOSITION = "Content-Disposition: "
		NAME                = "name=\""
		FILENAME            = "filename=\""
		CONTENT_TYPE        = "Content-Type: "
		CONTENT_LENGTH      = "Content-Length: "
	)
	result := make(map[string]string)
	for _, item := range arr {

		tarr := bytes.Split(item, []byte(";"))
		if len(tarr) != 2 {
			continue
		}

		tbyte := tarr[1]
		fmt.Println(string(tbyte))
		tbyte = bytes.ReplaceAll(tbyte, []byte("\r\n--"), []byte(""))
		tbyte = bytes.ReplaceAll(tbyte, []byte("name=\""), []byte(""))
		tempArr := bytes.Split(tbyte, []byte("\"\r\n\r\n"))
		if len(tempArr) != 2 {
			continue
		}
		bytes.HasPrefix(item, []byte("name="))
		result[strings.TrimSpace(string(tempArr[0]))] = strings.TrimSpace(string(tempArr[1]))
	}
	// for _, item := range arr {
	// 	if bytes.HasPrefix(item, []byte(CONTENT_DISPOSITION)) {
	// 		l := len(CONTENT_DISPOSITION)
	// 		arr1 := bytes.Split(item[l:], []byte("; "))
	// 		out_header.ContentDisposition = string(arr1[0])
	// 		if bytes.HasPrefix(arr1[1], []byte(NAME)) {
	// 			out_header.Name = string(arr1[1][len(NAME) : len(arr1[1])-1])
	// 		}
	// 		l = len(arr1[2])
	// 		if bytes.HasPrefix(arr1[2], []byte(FILENAME)) && arr1[2][l-1] == 0x22 {
	// 			out_header.FileName = string(arr1[2][len(FILENAME) : l-1])
	// 		}
	// 	} else if bytes.HasPrefix(item, []byte(CONTENT_TYPE)) {
	// 		l := len(CONTENT_TYPE)
	// 		out_header.ContentType = string(item[l:])
	// 	} else if bytes.HasPrefix(item, []byte(CONTENT_LENGTH)) {
	// 		l := len(CONTENT_LENGTH)
	// 		s := string(item[l:])
	// 		content_length, err := strconv.ParseInt(s, 10, 64)
	// 		if err != nil {
	// 			log.Printf("content length error:%s", string(item))
	// 			return out_header, false
	// 		} else {
	// 			out_header.ContentLength = content_length
	// 		}
	// 	} else {
	// 		log.Printf("unknown:%s\n", string(item))
	// 	}
	// }
	//fmt.Println(result)
	// if len(out_header.FileName) == 0 {
	// 	return out_header, false
	// }
	return result, true
}

func ReadToBoundary(boundary []byte, stream io.ReadCloser, target io.WriteCloser) ([]byte, bool, error) {
	read_data := make([]byte, 1024*8)
	read_data_len := 0
	buf := make([]byte, 1024*4)
	b_len := len(boundary)
	reach_end := false
	for !reach_end {
		read_len, err := stream.Read(buf)
		if err != nil {
			if err != io.EOF && read_len <= 0 {
				return nil, true, err
			}
			reach_end = true
		}

		copy(read_data[read_data_len:], buf[:read_len])
		read_data_len += read_len
		if read_data_len < b_len+4 {
			continue
		}
		loc := bytes.Index(read_data[:read_data_len], boundary)
		if loc >= 0 {

			target.Write(read_data[:loc-4])
			return read_data[loc:read_data_len], reach_end, nil
		}
		target.Write(read_data[:read_data_len-b_len-4])
		copy(read_data[0:], read_data[read_data_len-b_len-4:])
		read_data_len = b_len + 4
	}
	target.Write(read_data[:read_data_len])
	return nil, reach_end, nil
}

func ParseFromHead(read_data []byte, read_total int, boundary []byte, stream io.ReadCloser) (map[string]string, []byte, error) {

	buf := make([]byte, 1024*8)
	found_boundary := false
	boundary_loc := -1

	for {
		read_len, err := stream.Read(buf)
		if err != nil {
			if err != io.EOF {
				return nil, nil, err
			}
			break
		}
		if read_total+read_len > cap(read_data) {
			return nil, nil, fmt.Errorf("not found boundary")
		}
		copy(read_data[read_total:], buf[:read_len])
		read_total += read_len
		if !found_boundary {
			boundary_loc = bytes.LastIndex(read_data[:read_total], boundary)
			if boundary_loc == -1 {
				continue
			}
			found_boundary = true
		}
		start_loc := boundary_loc + len(boundary)
		fmt.Println(string(read_data))
		file_head_loc := bytes.Index(read_data[start_loc:read_total], []byte("\r\n\r\n"))
		if file_head_loc == -1 {
			continue
		}
		file_head_loc += start_loc
		ret := false
		headMap, ret := ParseFileHeader(read_data, boundary)
		if !ret {
			return headMap, nil, fmt.Errorf("ParseFileHeader fail:%s", string(read_data[start_loc:file_head_loc]))
		}
		return headMap, read_data[file_head_loc+4 : read_total], nil
	}
	return nil, nil, fmt.Errorf("reach to sream EOF")
}
