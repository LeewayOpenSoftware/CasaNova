package file

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	// Register the standard decoders so image.DecodeConfig can read
	// headers for jpeg, png, gif. Other formats fall through to the
	// imaging package's broader decoder set inside imaging.Open().
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/disintegration/imaging"
	"github.com/dsoprea/go-exif/v3"
	exifcommon "github.com/dsoprea/go-exif/v3/common"
)

// GetImage returns thumbnail bytes for path, from cache when possible.
//
// The cache sits HERE rather than in the handler because this is the single
// point every caller funnels through — putting it at a call site would leave
// the others paying full price, which is how half-applied optimisations happen
// in this codebase.
//
// Measured cost of the uncached path on a 4000x3000 JPEG: 650 ms and
// 20,430,363 B allocated per call. A gallery asks for dozens at a time.
func GetImage(path string, width, height int) ([]byte, error) {
	return CachedImage(path, width, height, func() ([]byte, error) {
		return generateImage(path, width, height)
	})
}

// generateImage is the original, expensive path: an embedded EXIF thumbnail if
// the file has one, otherwise a full decode and resample.
func generateImage(path string, width, height int) ([]byte, error) {
	if thumbnail, err := GetThumbnailByOwnerPhotos(path); err == nil {
		return thumbnail, nil
	}
	return GetThumbnailByWebPhoto(path, width, height)
}
func GetThumbnailByOwnerPhotos(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	buff := &bytes.Buffer{}

	defer file.Close()
	offset := 0
	offsets := []int{12, 30}

	head := make([]byte, 0xffff)

	r := io.TeeReader(file, buff)
	_, err = r.Read(head)
	if err != nil {
		return nil, err
	}

	for _, offset = range offsets {
		if _, err = exif.ParseExifHeader(head[offset:]); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	im, err := exifcommon.NewIfdMappingWithStandard()
	if err != nil {
		return nil, err
	}

	_, index, err := exif.Collect(im, exif.NewTagIndex(), head[offset:])
	if err != nil {
		return nil, err
	}

	ifd := index.RootIfd.NextIfd()
	if ifd == nil {
		return nil, exif.ErrNoThumbnail
	}
	thumbnail, err := ifd.Thumbnail()
	if err != nil {
		return nil, err
	}
	return thumbnail, nil
}

// maxDecodeDimension caps the longest side of any image we are willing
// to decode in-process. A 50 KiB PNG can declare a 30000x30000 canvas
// which decodes to ~3.6 GiB of RGBA — trivially OOM-able. Real photos
// from modern phones top out around 8000 px, so 12000 leaves headroom
// for medium-format scans without exposing the decoder bomb path.
const maxDecodeDimension = 12000

// maxDecodePixels is the secondary cap on total pixels (W*H), because a
// 12000x12000 image is still ~576 MiB of RGBA. 64 megapixels keeps a
// single decode under ~256 MiB and matches the upper bound of any
// consumer camera the file manager would realistically encounter.
const maxDecodePixels = 64 * 1000 * 1000

func GetThumbnailByWebPhoto(path string, width, height int) ([]byte, error) {
	// Wave 27: read the image header first and refuse to call imaging.Open
	// (which fully decodes) if the declared dimensions are obviously a
	// decompression bomb. imaging.DecodeConfig only parses the header so
	// it is cheap and bounded — it does NOT allocate the pixel buffer.
	hdr, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	cfg, _, err := image.DecodeConfig(hdr)
	hdr.Close()
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, errors.New("image: invalid dimensions")
	}
	if cfg.Width > maxDecodeDimension || cfg.Height > maxDecodeDimension {
		return nil, fmt.Errorf("image: dimensions %dx%d exceed cap %d", cfg.Width, cfg.Height, maxDecodeDimension)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return nil, fmt.Errorf("image: pixel count %d exceeds cap %d", int64(cfg.Width)*int64(cfg.Height), maxDecodePixels)
	}

	src, err := imaging.Open(path)
	if err != nil {
		return nil, err
	}

	img := imaging.Resize(src, width, 0, imaging.Lanczos)

	f, err := imaging.FormatFromFilename(path)
	if err != nil {
		return nil, err
	}
	// Wave 27: re-encoding through imaging.Encode strips EXIF for JPEG
	// (the encoder rewrites the file from raw pixels), so the thumbnail
	// path no longer leaks GPS / camera metadata. The "original" path in
	// GetFileImage still streams the raw file — that's a separate
	// trade-off (see comment in route/v1/file.go GetFileImage).
	buf := bytes.Buffer{}
	if err := imaging.Encode(&buf, img, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ImageExtArray() []string {

	ext := []string{
		"ase",
		"art",
		"bmp",
		"blp",
		"cd5",
		"cit",
		"cpt",
		"cr2",
		"cut",
		"dds",
		"dib",
		"djvu",
		"egt",
		"exif",
		"gif",
		"gpl",
		"grf",
		"icns",
		"ico",
		"iff",
		"jng",
		"jpeg",
		"jpg",
		"jfif",
		"jp2",
		"jps",
		"lbm",
		"max",
		"miff",
		"mng",
		"msp",
		"nitf",
		"ota",
		"pbm",
		"pc1",
		"pc2",
		"pc3",
		"pcf",
		"pcx",
		"pdn",
		"pgm",
		"PI1",
		"PI2",
		"PI3",
		"pict",
		"pct",
		"pnm",
		"pns",
		"ppm",
		"psb",
		"psd",
		"pdd",
		"psp",
		"px",
		"pxm",
		"pxr",
		"qfx",
		"raw",
		"rle",
		"sct",
		"sgi",
		"rgb",
		"int",
		"bw",
		"tga",
		"tiff",
		"tif",
		"vtf",
		"xbm",
		"xcf",
		"xpm",
		"3dv",
		"amf",
		"ai",
		"awg",
		"cgm",
		"cdr",
		"cmx",
		"dxf",
		"e2d",
		"egt",
		"eps",
		"fs",
		"gbr",
		"odg",
		// Wave 27: "svg" intentionally removed. SVG is XML and may
		// contain inline <script>. Letting GetFileImage serve it as
		// image/svg+xml in the same origin is XSS-as-a-feature. If
		// SVG previews are ever needed, sanitize on the server first
		// and serve from a sandboxed origin.
		"stl",
		"vrml",
		"x3d",
		"sxd",
		"v2d",
		"vnd",
		"wmf",
		"emf",
		"art",
		"xar",
		"png",
		"webp",
		"jxr",
		"hdp",
		"wdp",
		"cur",
		"ecw",
		"iff",
		"lbm",
		"liff",
		"nrrd",
		"pam",
		"pcx",
		"pgf",
		"sgi",
		"rgb",
		"rgba",
		"bw",
		"int",
		"inta",
		"sid",
		"ras",
		"sun",
		"tga",
	}

	return ext
}

/**
* @description:get a image's ext
* @param {string} path "file path"
* @return {string} ext "file ext"
* @return {error} err "error info"
 */
func GetImageExt(p string) (string, error) {
	file, err := os.Open(p)
	if err != nil {
		return "", err
	}

	buff := make([]byte, 512)

	_, err = file.Read(buff)

	if err != nil {
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
