package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mullsmith/internal/managed"
)

func TestDisplayImageSnapshotsRasterForInlinePresentation(t *testing.T) {
	root := toolRoot(t)
	writeUpload(t, root, "work/chart.png", pngBytesFor(64))
	display := DisplayImage{Files: managed.New(1<<20, 1000), MaxImageBytes: 1 << 20}

	res, err := run(t, display, root, argsJSON(t, map[string]string{
		"path": "work/chart.png", "caption": "Quarterly results",
	}))
	if err != nil {
		t.Fatalf("display_image: %v", err)
	}
	data, _ := res.Data.(map[string]any)
	image, _ := data["image"].(map[string]any)
	if image["path"] != "artifacts/chart.png" ||
		image["url"] != "/api/v1/files/image?path=artifacts%2Fchart.png" ||
		image["caption"] != "Quarterly results" || image["media_type"] != "image/png" {
		t.Fatalf("image display data = %#v", image)
	}
	if _, err := os.Stat(filepath.Join(root, toolUser, "artifacts/chart.png")); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	if !strings.Contains(res.Content, `"displayed":"artifacts/chart.png"`) {
		t.Errorf("model-facing result = %s", res.Content)
	}
}

func TestDisplayImageRejectsUnsafeOrUnsuitableFiles(t *testing.T) {
	root := toolRoot(t)
	writeUpload(t, root, "work/vector.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	writeUpload(t, root, "work/large.png", pngBytesFor(64))
	for _, tc := range []struct {
		path, want string
		max        int64
	}{
		{"work/vector.svg", "PNG, JPEG or WebP", 1 << 20},
		{"work/large.png", "display size cap", 16},
	} {
		display := DisplayImage{Files: managed.New(1<<20, 1000), MaxImageBytes: tc.max}
		if _, err := run(t, display, root, argsJSON(t, map[string]string{"path": tc.path})); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("display_image(%q) error = %v, want %q", tc.path, err, tc.want)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, toolUser, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("refused images created artifacts: %v", entries)
	}
}

func TestDisplayImageSnapshotsUploadAndLeavesOriginal(t *testing.T) {
	root := toolRoot(t)
	original := pngBytesFor(64)
	writeUpload(t, root, "uploads/photo.png", original)
	display := DisplayImage{Files: managed.New(1<<20, 1000), MaxImageBytes: 1 << 20}

	res, err := run(t, display, root, argsJSON(t, map[string]string{"path": "uploads/photo.png"}))
	if err != nil {
		t.Fatalf("display uploaded image: %v", err)
	}
	data := res.Data.(map[string]any)
	image := data["image"].(map[string]any)
	if image["path"] != "artifacts/photo.png" {
		t.Fatalf("display path = %v", image["path"])
	}
	for _, rel := range []string{"uploads/photo.png", "artifacts/photo.png"} {
		got, err := os.ReadFile(filepath.Join(root, toolUser, filepath.FromSlash(rel)))
		if err != nil || string(got) != string(original) {
			t.Errorf("%s after display = %q, %v", rel, got, err)
		}
	}
}
