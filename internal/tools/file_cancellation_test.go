package tools

import (
	"context"
	"errors"
	"mullsmith/internal/managed"
	"os"
	"path/filepath"
	"testing"
)

func TestCancelledFileToolsLeaveFilesUntouched(t *testing.T) {
	root := toolRoot(t)
	writeDirect(t, root, "work/keep.txt", "original")
	files := managed.New(1 << 20)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tool := range []Tool{FsList{}, FsRead{}, FsWrite{Files: files}, FsDelete{}, FilesShare{Files: files}, ViewImage{}} {
		t.Run(tool.Name(), func(t *testing.T) {
			_, err := tool.Execute(ctx, inv(root, `{"path":"work/keep.txt","content":"replacement"}`))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled tool returned %v", err)
			}
		})
	}
	data, err := os.ReadFile(filepath.Join(root, toolUser, "work", "keep.txt"))
	if err != nil || string(data) != "original" {
		t.Fatalf("original modified: %q %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, toolUser, "artifacts"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("artifact created: %v %v", entries, err)
	}
}
