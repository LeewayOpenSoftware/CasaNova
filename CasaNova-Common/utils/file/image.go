package file

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Image file utilities, consolidated from CasaNova and CasaNova-UserService.
// These functions deal with detecting image types by content or extension —
// they do NOT use image/png or image/jpeg decoders, so there's no heavy
// dependency cost from living in Common.

// ImageExtArray returns the list of recognized image file extensions.
// Used for validating user uploads and scanning shared folders for images.
//
// Wave 27: "svg" deliberately removed. SVG is XML and can carry inline
// <script>. Treating it as a normal image causes the preview endpoint to
// serve it as image/svg+xml in the user's same-origin session, which is
// stored XSS. If SVG support is ever wanted, sanitize server-side first.
func ImageExtArray() []string {
	return []string{
		"ase", "art", "bmp", "blp", "cd5", "cit", "cpt", "cr2", "cut", "dds",
		"dib", "djvu", "egt", "exif", "gif", "gpl", "grf", "icns", "ico", "iff",
		"jng", "jpeg", "jpg", "jfif", "jp2", "jps", "lbm", "max", "miff", "mng",
		"msp", "nef", "nitf", "ota", "pbm", "pc1", "pc2", "pc3", "pcf", "pcx",
		"pdn", "pgm", "PI1", "PI2", "PI3", "pict", "pct", "pnm", "pns", "ppm",
		"psb", "psd", "pdd", "psp", "px", "pxm", "pxr", "qfx", "raw", "rle",
		"sct", "sgi", "rgb", "int", "bw", "tga", "tiff", "tif", "vtf", "xbm",
		"xcf", "xpm", "3dv", "amf", "ai", "awg", "cgm", "cdr", "cmx", "dxf",
		"e2d", "egt", "eps", "fs", "gbr", "odg", "stl", "vrml", "x3d",
		"sxd", "v2d", "vnd", "wmf", "emf", "art", "xar", "png", "webp", "jxr",
		"hdp", "wdp", "cur", "ecw", "iff", "lbm", "liff", "nrrd", "pam", "pcx",
		"pgf", "sgi", "rgb", "rgba", "bw", "int", "inta", "sid", "ras", "sun",
		"tga",
	}
}

// GetImageExt inspects the first bytes of a file and returns its image
// extension. Returns an error if the file isn't a recognized image type.
func GetImageExt(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()

	buff := make([]byte, 512)
	if _, err = f.Read(buff); err != nil {
		return "", err
	}

	filetype := http.DetectContentType(buff)
	ext := ImageExtArray()
	for i := 0; i < len(ext); i++ {
		if strings.Contains(ext[i], filetype[6:]) {
			return ext[i], nil
		}
	}
	return "", errors.New("invalid image type")
}

// GetImageExtByName returns the image extension based on a file's name.
// Faster than GetImageExt but trusts the filename rather than content.
func GetImageExtByName(p string) (string, error) {
	extArr := ImageExtArray()
	ext := filepath.Ext(p)
	for i := 0; i < len(extArr); i++ {
		if strings.Contains(ext, extArr[i]) {
			return extArr[i], nil
		}
	}
	return "", errors.New("invalid image type")
}

// SaveUploadedFile writes a multipart file upload to disk at dst,
// creating intermediate directories as needed with restrictive permissions.
// Consolidated from CasaNova-UserService.
func SaveUploadedFile(file *multipart.FileHeader, dst string) error {
	src, err := file.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	if err = os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, src)
	return err
}
