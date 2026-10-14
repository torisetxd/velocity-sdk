package context

import (
	stdctx "context"
	"strings"
	"testing"
)

func TestFileRetentionNeedsFileReader(t *testing.T) {
	if strings.Contains(compressionUserRequest(false), "files_to_retain") {
		t.Fatal("prompt asks for files_to_retain without a FileReader")
	}
	if !strings.Contains(compressionUserRequest(true), "<files_to_retain>") {
		t.Fatal("prompt lost files_to_retain with a FileReader")
	}
	if files := LoadRetainedFiles(stdctx.Background(), ".", "compress.go", nil); files != nil {
		t.Fatalf("nil reader retained %v", files)
	}
	if files := LoadRetainedFiles(stdctx.Background(), ".", "compress.go", DirFileReader(".")); len(files) != 1 {
		t.Fatalf("DirFileReader retained %v", files)
	}
}
