package tools

// view_image lets the model see a picture that is already on the server.
// Tool answers are text — a picture cannot ride one — so Execute only
// validates the file and names it via Result.AttachPath; the chat loop
// then appends a synthetic user turn carrying the embedded image, exactly
// the pipeline an upload rides. That turn is persisted, so every later
// replay keeps the picture in view. Registration needs at least one
// llm.models entry spelling vision: true, and the tool is offered per
// request only to vision models: a text-only endpoint could not parse
// what the turn contains.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"mullsmith/internal/scratch"
)

// ViewImageName is exported because both the registry's auto-approval
// default and the server's attach pipeline refer to the tool by name.
const ViewImageName = "view_image"

// ViewImage attaches one picture from anywhere in the user's sandbox.
type ViewImage struct {
	// MaxImageBytes mirrors app_limits.vision_max_image_bytes: a file the
	// server-side embed would silently refuse is instead explained to
	// the model, which is the difference between retrying and wondering.
	MaxImageBytes int64
}

func (ViewImage) Name() string { return ViewImageName }

func (ViewImage) Description() string {
	return "Look at an image already on the server. Tool answers are text, so this " +
		"call attaches the picture itself: it joins the conversation as a new message " +
		"and you see it from the next turn onwards (and on every later turn — the picture " +
		"stays). Use it whenever the user asks about an image that is not already visible " +
		"to you inline: uploads/ files the user sent earlier, or pictures in work/ and " +
		"artifacts/ that you produced. Args: path (PNG/JPEG/WebP within the vision size " +
		"cap)."
}

func (ViewImage) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string",
				"description": `Relative path of the picture, e.g. "uploads/photo.png".`},
		},
		"required": []string{"path"},
	}
}

func (v ViewImage) Execute(ctx context.Context, inv Invocation) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	root, err := scratchRoot(inv)
	if err != nil {
		return Result{}, err
	}
	args, err := parseArgs[struct {
		Path string `json:"path"`
	}](inv.Arguments)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(args.Path) == "" {
		return Result{}, errors.New("path is required")
	}
	// Canonical rejoin doubles as validation (§9.7): traversal and the
	// rest never reach the open chain. Any sandbox dir is viewable —
	// uploads/, work/ and artifacts/ are all the user's own bytes, and
	// this turn names the real file rather than a copy. (A view of a
	// work/ or artifacts/ picture fades when the TTL sweep takes the
	// file; the attachment row then replays manifest-only, as uploads
	// deletions already do.)
	segs, err := scratch.ParseRel(args.Path)
	if err != nil {
		return Result{}, fsErr("view_image", err)
	}
	if len(segs) == 0 {
		return Result{}, errors.New("path is required")
	}
	rel := path.Join(segs...)

	fi, err := scratch.Stat(root, inv.UserID, rel)
	if err != nil {
		return Result{}, fsErr("view_image", err)
	}
	if !fi.Mode().IsRegular() {
		if fi.IsDir() {
			return Result{}, errors.New("path is a directory, not a file")
		}
		return Result{}, errors.New("path is not a regular file")
	}
	if v.MaxImageBytes > 0 && fi.Size() > v.MaxImageBytes {
		return Result{}, fmt.Errorf("image is %s, above the vision size cap of %s",
			humanBytes(fi.Size()), humanBytes(v.MaxImageBytes))
	}
	// The sniffed type decides, never the extension (§9.4): the bytes are
	// about to enter the model's context as a picture.
	if _, err := rasterImageType(root, inv.UserID, rel); err != nil {
		if errors.Is(err, errImageRead) || errors.Is(err, errUnsupportedImage) {
			return Result{}, err
		}
		return Result{}, fsErr("view_image", err)
	}

	body, _ := json.Marshal(map[string]any{
		"attached": rel,
		"note":     "the picture joins the conversation as the next message; you will see it from the next turn onwards",
	})
	return Result{
		Content:    string(body),
		Summary:    fmt.Sprintf("Attached %s to the conversation", quoteShort(rel)),
		AttachPath: rel,
	}, nil
}
