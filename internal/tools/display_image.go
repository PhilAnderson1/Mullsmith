package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"mullsmith/internal/managed"
	"mullsmith/internal/scratch"
)

const DisplayImageName = "display_image"

// DisplayImage presents a durable snapshot of a generated or downloaded
// raster image in the chat. It is intentionally separate from view_image:
// display_image is for the user, while view_image attaches bytes for the model.
type DisplayImage struct {
	Files         *managed.Service
	MaxImageBytes int64
}

func (DisplayImage) Name() string { return DisplayImageName }

func (DisplayImage) Description() string {
	return "Display a local image inline in the chat for the user. Use this when the " +
		"user should see a generated or downloaded picture without a download card. The " +
		"image is snapshotted into artifacts/ so the chat keeps the displayed version until " +
		"artifact expiry. Args: path (PNG/JPEG/WebP under uploads/, work/ or artifacts/), caption " +
		"(optional short text shown below it). This shows the image to the user; use " +
		"view_image instead when you need to inspect it yourself."
}

func (DisplayImage) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Relative path of the PNG, JPEG or WebP image."},
			"caption": map[string]any{"type": "string", "description": "Optional short caption shown below the image."},
		},
		"required": []string{"path"},
	}
}

func (d DisplayImage) Execute(ctx context.Context, inv Invocation) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	root, err := scratchRoot(inv)
	if err != nil {
		return Result{}, err
	}
	args, err := parseArgs[struct {
		Path    string `json:"path"`
		Caption string `json:"caption"`
	}](inv.Arguments)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(args.Path) == "" {
		return Result{}, errors.New("path is required")
	}
	segs, err := scratch.ParseRel(args.Path)
	if err != nil {
		return Result{}, fsErr(DisplayImageName, err)
	}
	if len(segs) < 2 || (segs[0] != "uploads" && segs[0] != "work" && segs[0] != "artifacts") {
		return Result{}, errors.New("display_image accepts files under uploads/, work/ or artifacts/")
	}
	rel := path.Join(segs...)
	fi, err := scratch.Stat(root, inv.UserID, rel)
	if err != nil {
		return Result{}, fsErr(DisplayImageName, err)
	}
	if !fi.Mode().IsRegular() {
		if fi.IsDir() {
			return Result{}, errors.New("path is a directory, not a file")
		}
		return Result{}, errors.New("path is not a regular file")
	}
	if d.MaxImageBytes > 0 && fi.Size() > d.MaxImageBytes {
		return Result{}, fmt.Errorf("image is %s, above the display size cap of %s",
			humanBytes(fi.Size()), humanBytes(d.MaxImageBytes))
	}
	mediaType, err := rasterImageType(root, inv.UserID, rel)
	if err != nil {
		if errors.Is(err, errImageRead) || errors.Is(err, errUnsupportedImage) {
			return Result{}, err
		}
		return Result{}, fsErr(DisplayImageName, err)
	}
	file, err := d.Files.DisplayImage(ctx, root, inv.UserID, rel)
	if err != nil {
		return Result{}, err
	}
	caption := Elide(args.Caption, 300)
	imageURL := "/api/v1/files/image?path=" + url.QueryEscape(file.Path)
	body, _ := json.Marshal(map[string]any{
		"displayed": file.Path,
		"caption":   caption,
	})
	return Result{
		Content: string(body),
		Summary: fmt.Sprintf("Displayed %s in the chat", quoteShort(file.Name)),
		Data: map[string]any{
			"image": map[string]any{
				"path": file.Path, "name": file.Name, "bytes": file.Bytes,
				"url": imageURL, "caption": caption, "media_type": mediaType,
			},
		},
	}, nil
}
