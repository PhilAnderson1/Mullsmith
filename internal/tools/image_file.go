package tools

import (
	"errors"
	"io"
	"net/http"

	"mullsmith/internal/scratch"
)

var (
	errImageRead        = errors.New("read failed")
	errUnsupportedImage = errors.New("file is not a PNG, JPEG or WebP image")
)

// rasterImageType verifies the bytes rather than trusting a filename. SVG is
// deliberately excluded: unlike these raster formats it can become an active
// document when opened directly in the application origin.
func rasterImageType(root, userID, rel string) (string, error) {
	f, err := scratch.OpenFile(root, userID, rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head, err := io.ReadAll(io.LimitReader(f, 512))
	if err != nil || len(head) == 0 {
		return "", errImageRead
	}
	switch mediaType := http.DetectContentType(head); mediaType {
	case "image/png", "image/jpeg", "image/webp":
		return mediaType, nil
	default:
		return "", errUnsupportedImage
	}
}
