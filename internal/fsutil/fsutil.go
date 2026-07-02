// Package fsutil 提供目录的流式 tar 打包/解包，供 hub 与 agent 在 upload/download
// 的 is_dir 模式下共用。两个核心约束：
//
//  1. 零临时 tar 文件 —— TarDir 返回的 io.Reader 一边 walk 一边写 tar 流，调用方
//     一边读；UntarStream 一边读 tar 一边落地真实目录树。内存占用 O(一个 tar block)。
//
//  2. 路径安全 —— UntarStream 对每个 entry 做路径清洗，强制限定在 root 下，拒绝
//     `..` 逃逸、绝对路径、以及指向 root 外的符号链接（CVE-2019-6111 教训）。Go 标准库
//     archive/tar 本身不做这些校验，是应用层责任。
package fsutil

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// TarResult 在 tar 流读尽后由后台 goroutine 填充，调用方通过 Result() 等待并读取。
type TarResult struct {
	Size     int64  // tar 字节流的字节数（含 header）
	Sha256   string // 全 tar 字节流的 sha256
	NEntries int    // 写入的 entry 数（含目录）
	Err      error  // 打包错误；非 nil 时其余字段无意义
	done     chan struct{}
}

// TarDir 把 root 目录流式打包成 tar。返回一个 io.Reader（tar 字节流）和 *TarResult。
// root 必须存在且是目录。调用方读尽 reader 后调用 res.Result() 拿最终统计。
// 若调用方在 reader 读尽前放弃，后台 goroutine 会因 Pipe 写阻塞而悬挂，直到调用方
// Close 或 GC 回收 —— 调用方应在出错路径上丢弃 reader 并接受 goroutine 最终被 GC。
func TarDir(root string) (io.Reader, *TarResult, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, fmt.Errorf("fsutil: stat %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("fsutil: %q is not a directory", root)
	}

	pr, pw := io.Pipe()
	res := &TarResult{done: make(chan struct{})}

	go func() {
		size, sha, n, err := tarDirTo(root, pw)
		if err != nil {
			pw.CloseWithError(err)
			res.Err = err
		} else {
			pw.Close()
			res.Size, res.Sha256, res.NEntries = size, sha, n
		}
		close(res.done)
	}()

	return pr, res, nil
}

// Result 阻塞直到 tar 流写尽，返回最终统计。多次调用返回同一结果（幂等）。
func (r *TarResult) Result() (size int64, sha string, nEntries int, err error) {
	<-r.done
	return r.Size, r.Sha256, r.NEntries, r.Err
}

// tarDirTo 把 root 流式打包写入 w，返回 (流字节数, 流 sha256, entry 数, err)。
func tarDirTo(root string, w io.Writer) (int64, string, int, error) {
	h := sha256.New()
	counter := &countingWriter{}
	mw := io.MultiWriter(w, h, counter)

	nEntries := 0

	tw := tar.NewWriter(mw)
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return fmt.Errorf("rel %q under %q: %w", path, root, rerr)
		}
		if rel == "." {
			// root 本身不入 tar —— remote_path 已作为容器目录存在。
			return nil
		}
		name := filepath.ToSlash(rel)

		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			l, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			link = l
		}

		hdr, herr := tar.FileInfoHeader(info, link)
		if herr != nil {
			return fmt.Errorf("header for %q: %w", path, herr)
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		nEntries++

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			f, ferr := os.Open(path)
			if ferr != nil {
				return ferr
			}
			_, cerr := io.Copy(tw, f)
			f.Close()
			if cerr != nil {
				return cerr
			}
		case tar.TypeSymlink, tar.TypeLink, tar.TypeDir:
			// 无 body
		default:
			// 设备/fifo/socket 等不打包内容，仅留 header。
		}
		return nil
	})
	if walkErr != nil {
		tw.Close()
		return 0, "", 0, walkErr
	}
	if err := tw.Close(); err != nil {
		return 0, "", 0, err
	}
	return counter.n, hex.EncodeToString(h.Sum(nil)), nEntries, nil
}

// countingWriter 累计写入字节数。
type countingWriter struct {
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// --- Untar ---

// UntarResult 是 UntarStream 的返回值。
type UntarResult struct {
	Size    int64    // 解包消耗的 tar 字节数
	Sha256  string   // 全 tar 字节流的 sha256
	Entries []string // 已成功落地的相对路径（含目录条目）
	Err     error    // 第一个错误；非 nil 时仍返回已写部分 Entries
}

// UntarStream 把 r 上的 tar 字节流解包到 root 目录下，并累计全流 sha256/size。
// 安全：每个 entry 路径强制限定在 root 下；拒绝绝对路径、..逃逸、指向 root 外的符号链接。
// root 不存在则按 0755 创建；存在必须是目录。
func UntarStream(root string, r io.Reader) *UntarResult {
	res := &UntarResult{}
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		res.Err = fmt.Errorf("fsutil: abs root: %w", err)
		return res
	}
	if info, statErr := os.Stat(absRoot); os.IsNotExist(statErr) {
		if mkErr := os.MkdirAll(absRoot, 0o755); mkErr != nil {
			res.Err = fmt.Errorf("fsutil: mkdir root: %w", mkErr)
			return res
		}
	} else if statErr != nil {
		res.Err = fmt.Errorf("fsutil: stat root: %w", statErr)
		return res
	} else if !info.IsDir() {
		res.Err = fmt.Errorf("fsutil: %q exists and is not a directory", absRoot)
		return res
	}

	h := sha256.New()
	cr := &countingReader{r: io.TeeReader(r, h)}

	tr := tar.NewReader(cr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			res.Err = fmt.Errorf("untar: read header: %w", err)
			res.Size = cr.n
			return res
		}
		if err := untarEntry(absRoot, hdr, tr, res); err != nil {
			res.Err = fmt.Errorf("untar %q: %w", hdr.Name, err)
			res.Size = cr.n
			return res
		}
	}
	res.Sha256 = hex.EncodeToString(h.Sum(nil))
	res.Size = cr.n
	return res
}

func untarEntry(absRoot string, hdr *tar.Header, tr *tar.Reader, res *UntarResult) error {
	name := filepath.Clean(hdr.Name)
	if filepath.IsAbs(name) {
		return fmt.Errorf("entry %q is absolute path", hdr.Name)
	}
	target := filepath.Join(absRoot, name)
	if !isWithinRoot(target, absRoot) {
		return fmt.Errorf("entry %q escapes root", hdr.Name)
	}
	rel, _ := filepath.Rel(absRoot, target)

	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, os.FileMode(hdr.Mode).Perm()|0o700); err != nil {
			return err
		}
		res.Entries = append(res.Entries, rel+"/")
	case tar.TypeReg, tar.TypeRegA:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		f.Close()
		res.Entries = append(res.Entries, rel)
	case tar.TypeSymlink:
		if isEscapingLink(target, hdr.Linkname, absRoot) {
			return fmt.Errorf("symlink %q -> %q escapes root", hdr.Name, hdr.Linkname)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		_ = os.Remove(target)
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return err
		}
		res.Entries = append(res.Entries, rel)
	case tar.TypeLink:
		linkTarget := filepath.Join(absRoot, filepath.Clean(hdr.Linkname))
		if !isWithinRoot(linkTarget, absRoot) {
			return fmt.Errorf("hardlink %q -> %q escapes root", hdr.Name, hdr.Linkname)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		_ = os.Remove(target)
		if err := os.Link(linkTarget, target); err != nil {
			return err
		}
		res.Entries = append(res.Entries, rel)
	default:
		// 设备/fifo/socket 等不支持，忽略（避免恶意 tar 用设备文件攻击）。
	}
	return nil
}

// isWithinRoot 返回 path（已 Clean、绝对）是否在 root（已 Clean、绝对）之下。
func isWithinRoot(path, root string) bool {
	if path == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(path, prefix)
}

// isEscapingLink 判断 symlink target 解析后是否跑出 absRoot。
func isEscapingLink(linkPath, link, absRoot string) bool {
	var resolved string
	if filepath.IsAbs(link) {
		resolved = filepath.Clean(link)
	} else {
		resolved = filepath.Join(filepath.Dir(linkPath), link)
	}
	return !isWithinRoot(resolved, absRoot)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
