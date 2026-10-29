package tools

import (
	"strings"
	"testing"

	"mullsmith/internal/scratch"
)

// pngBytesFor produces bytes whose SNIFFED type is image/png (the signature
// is all http.DetectContentType keys on) padded to a minimum size.
func pngBytesFor(body int) []byte {
	b := []byte("\x89PNG\r\n\x1a\n")
	return append(b, make([]byte, body)...)
}

func writeUpload(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	f, err := scratch.CreateFile(root, toolUser, rel, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestViewImageAttachesPictureFromAnySandboxDir(t *testing.T) {
	root := toolRoot(t)
	writeUpload(t, root, "uploads/photo.png", pngBytesFor(64))
	writeUpload(t, root, "work/diagram.png", pngBytesFor(64))
	writeUpload(t, root, "artifacts/out/chart.png", pngBytesFor(64))

	for _, rel := range []string{"uploads/photo.png", "work/diagram.png", "artifacts/out/chart.png"} {
		res, err := run(t, ViewImage{MaxImageBytes: 1 << 20}, root,
			argsJSON(t, map[string]string{"path": rel}))
		if err != nil {
			t.Fatalf("view_image %s: %v", rel, err)
		}
		if res.AttachPath != rel {
			t.Errorf("AttachPath = %q, want %q", res.AttachPath, rel)
		}
		if !strings.Contains(res.Content, `"`+rel+`"`) {
			t.Errorf("result content = %s", res.Content)
		}
	}
}

func TestViewImageRefusals(t *testing.T) {
	root := toolRoot(t)
	writeUpload(t, root, "uploads/notes.txt", []byte("just text, no picture"))
	writeUpload(t, root, "uploads/tiny.png", pngBytesFor(64))

	for _, tc := range []struct {
		name, path, want string
		max              int64
	}{
		{"path escape", "uploads/../work/diagram.png", "outside the scratchpad sandbox", 1 << 20},
		{"bare directory", "uploads", "directory", 1 << 20},
		{"missing file", "uploads/gone.png", "not found", 1 << 20},
		{"non-raster content", "uploads/notes.txt", "PNG, JPEG or WebP", 1 << 20},
		{"over the size cap", "uploads/tiny.png", "vision size cap", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := run(t, ViewImage{MaxImageBytes: tc.max}, root,
				argsJSON(t, map[string]string{"path": tc.path}))
			if err == nil {
				t.Fatalf("want error, got %+v", res)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			if res.AttachPath != "" {
				t.Errorf("AttachPath set on refusal: %q", res.AttachPath)
			}
		})
	}
}
