package fsutil

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// writeTree 写一组相对路径 -> 内容到 dir 下，自动建父目录。
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", p, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir parent %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
}

// walkRel 返回 dir 下所有相对路径（用正斜杠），已排序。
func walkRel(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

func TestTarDir_RoundTrip(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{
		"a.txt":           "hello a",
		"sub/":            "",
		"sub/b.txt":       "hello b",
		"sub/deep/c.txt":  "deep c",
	})

	r, res, err := TarDir(src)
	if err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	// 完整读出 tar 流再喂给 UntarStream（模拟传输链路）。
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("copy tar stream: %v", err)
	}
	size, sha, nEntries, terr := res.Result()
	if terr != nil {
		t.Fatalf("TarResult: %v", terr)
	}
	if size != int64(buf.Len()) {
		t.Errorf("size=%d want=%d", size, buf.Len())
	}
	if nEntries == 0 {
		t.Errorf("expected >0 entries, got %d", nEntries)
	}

	dst := t.TempDir()
	ures := UntarStream(filepath.Join(dst, "src"), &buf)
	if ures.Err != nil {
		t.Fatalf("UntarStream: %v", ures.Err)
	}
	if ures.Sha256 != sha {
		t.Errorf("sha mismatch: tar=%s untar=%s", sha, ures.Sha256)
	}
	if ures.Size != size {
		t.Errorf("untar size=%d want=%d", ures.Size, size)
	}

	// 校验内容
	gotA, _ := os.ReadFile(filepath.Join(dst, "src", "a.txt"))
	if string(gotA) != "hello a" {
		t.Errorf("a.txt = %q", gotA)
	}
	gotC, _ := os.ReadFile(filepath.Join(dst, "src", "sub", "deep", "c.txt"))
	if string(gotC) != "deep c" {
		t.Errorf("c.txt = %q", gotC)
	}
}

func TestTarDir_EmptyDir(t *testing.T) {
	src := t.TempDir()
	r, res, err := TarDir(src)
	if err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	var buf bytes.Buffer
	io.Copy(&buf, r)
	if _, _, _, terr := res.Result(); terr != nil {
		t.Fatalf("Result: %v", terr)
	}
	dst := t.TempDir()
	ures := UntarStream(filepath.Join(dst, "empty"), &buf)
	if ures.Err != nil {
		t.Fatalf("UntarStream empty: %v", ures.Err)
	}
}

func TestTarDir_NotADir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file.txt")
	os.WriteFile(f, []byte("x"), 0o644)
	_, _, err := TarDir(f)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected not-a-directory error, got %v", err)
	}
}

func TestUntarStream_PathEscape_DotDot(t *testing.T) {
	// 构造一个含 ../ 的恶意 tar
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "../evil.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len("pwned"))})
	tw.Write([]byte("pwned"))
	tw.Close()

	root := t.TempDir()
	res := UntarStream(root, &buf)
	if res.Err == nil {
		t.Fatalf("expected error, got nil; entries=%v", res.Entries)
	}
	// 确认 evil.txt 没落地到 root 的上级
	if _, err := os.Stat(filepath.Join(root, "..", "evil.txt")); err == nil {
		t.Fatalf("evil.txt escaped!")
	}
}

func TestUntarStream_PathEscape_Absolute(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "/tmp/evil-abs.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 0})
	tw.Close()

	root := t.TempDir()
	res := UntarStream(root, &buf)
	if res.Err == nil {
		t.Fatalf("expected absolute-path rejection, got nil")
	}
}

func TestUntarStream_EscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "evil-link", Typeflag: tar.TypeSymlink, Linkname: "../../../etc/passwd", Mode: 0777})
	tw.Close()

	root := t.TempDir()
	res := UntarStream(root, &buf)
	if res.Err == nil {
		t.Fatalf("expected escaping-symlink rejection, got nil")
	}
	if _, err := os.Lstat(filepath.Join(root, "evil-link")); err == nil {
		t.Fatalf("escaping symlink was created")
	}
}

func TestUntarStream_SafeSymlink_Inside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "target.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len("data"))})
	tw.Write([]byte("data"))
	tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "target.txt", Mode: 0777})
	tw.Close()

	root := t.TempDir()
	res := UntarStream(root, &buf)
	if res.Err != nil {
		t.Fatalf("safe symlink should pass, got %v", res.Err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("safe symlink not created: %v", err)
	}
}

func TestUntarStream_CreatesRootIfMissing(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "x.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()

	root := filepath.Join(t.TempDir(), "nested", "deep")
	res := UntarStream(root, &buf)
	if res.Err != nil {
		t.Fatalf("expected root auto-create, got %v", res.Err)
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); err != nil {
		t.Fatalf("x.txt not created: %v", err)
	}
}

func TestTarDir_DeepNesting(t *testing.T) {
	src := t.TempDir()
	// 建一个 5 层深的目录
	p := filepath.Join(src, "a", "b", "c", "d", "e")
	os.MkdirAll(p, 0o755)
	os.WriteFile(filepath.Join(p, "leaf.txt"), []byte("deep"), 0o644)

	r, res, err := TarDir(src)
	if err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	var buf bytes.Buffer
	io.Copy(&buf, r)
	if _, _, _, terr := res.Result(); terr != nil {
		t.Fatalf("Result: %v", terr)
	}
	dst := t.TempDir()
	if ures := UntarStream(filepath.Join(dst, "src"), &buf); ures.Err != nil {
		t.Fatalf("UntarStream deep: %v", ures.Err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "src", "a", "b", "c", "d", "e", "leaf.txt"))
	if string(got) != "deep" {
		t.Errorf("leaf = %q", got)
	}
}

func TestUntarStream_PartialEntriesOnError(t *testing.T) {
	// 第一个 entry 合法，第二个绝对路径 -> 应报错且 Entries 含第一个
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "ok.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 2})
	tw.Write([]byte("ok"))
	tw.WriteHeader(&tar.Header{Name: "/abs/bad.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()

	root := t.TempDir()
	res := UntarStream(root, &buf)
	if res.Err == nil {
		t.Fatalf("expected error")
	}
	if len(res.Entries) != 1 || res.Entries[0] != "ok.txt" {
		t.Errorf("Entries=%v, want [ok.txt]", res.Entries)
	}
}
