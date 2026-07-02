// agent_fs_chunk.go — 分块文件传输的 agent 端状态机，支持 upload/download 的 is_dir 模式。
//
// 单文件(is_dir=false) 上传: open 在目标目录建临时文件 -> chunk 逐块按 offset 写
// (写入前校验块 sha256, 不符则不写并回 OK=false 让调用方只重传该块) -> commit 重读
// 临时文件算整体 sha256 + 校验大小, 通过则 os.Rename 原子替换目标 + chmod; 任一环节失败
// 删除临时文件, 目标不被触碰。
//
// 目录(is_dir=true) 上传: open 在目标父目录下建 .dbg-uploaddir-* 临时目录 + io.Pipe,
// 启 goroutine 跑 fsutil.UntarStream 消费管道读端; chunk 写向管道写端(逐块 sha256 校验
// 保留, 同时累计全流 sha) -> commit 关闭管道写端, 等 untar 完成, 用累计的流 sha/size 比
// WantSha/WantSize, 通过则 rename 临时目录成目标 path; 失败则删除整个临时目录, commit
// 结果回已落地 Entries。源端权威: hub 决定传输模式; agent 不二次 stat(upload 时)。
//
// 单文件下载: open 流式算 size + sha256 并开会话 -> chunk 按 offset 从原文件读一块 +
// 块 sha256 + EOF。目录下载: open 启 goroutine 跑 fsutil.TarDir 写向管道写端(流式算
// sha/size/entries), session 持有管道读端 -> chunk 从管道读一块(末块带 EOF)。会话 id
// 走 wire.Envelope.Sid; 每块独立 Seq。空闲会话超时后自动清理, 防泄漏。
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"debugmcp/internal/fsutil"
	"debugmcp/internal/wire"
)

const (
	defaultChunkSize int64 = 4 << 20 // 4 MiB
	fsSessionTimeout       = 10 * time.Minute
)

// chunkedUpload 是 agent 端一次上传会话。chunkSink 抽象使单文件(os.File)与目录(untar 管道写端)
// 复用同一 chunk 写入路径。
type chunkedUpload struct {
	id      string
	path    string
	mode    os.FileMode
	isDir   bool
	wantSha string
	mu      sync.Mutex
	lastAct time.Time

	// 单文件模式
	f *os.File

	// 目录模式
	pipeW       *io.PipeWriter
	untarResult chan *fsutil.UntarResult // untar goroutine 完成后发结果
	partialDir  string                   // .dbg-uploaddir-* 临时目录; commit 通过则 rename 成 path
	sha         hash.Hash                // 目录模式累计流字节 sha
}

// chunkedDownload 是 agent 端一次下载会话。
type chunkedDownload struct {
	id       string
	path     string
	isDir    bool
	total    int64
	wantSha  string
	chunkSz  int64
	nEntries int
	mu       sync.Mutex
	lastAct  time.Time

	// 目录模式: tar 流来自 pipeR。
	pipeR *io.PipeReader
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- upload ---

func (a *Agent) fsWriteOpen(env *wire.Envelope, req *wire.FSWriteOpen) {
	mode := os.FileMode(req.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if req.IsDir {
		a.fsWriteOpenDir(env, req, mode)
		return
	}
	a.fsWriteOpenFile(env, req, mode)
}

func (a *Agent) fsWriteOpenFile(env *wire.Envelope, req *wire.FSWriteOpen, mode os.FileMode) {
	f, err := os.CreateTemp(filepath.Dir(req.Path), ".dbg-upload-*")
	if err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteOpenResult, &wire.FSWriteOpenResult{Err: err.Error()})
		return
	}
	up := &chunkedUpload{
		id: f.Name(), path: req.Path, mode: mode, wantSha: req.TotalSha256,
		f: f, lastAct: time.Now(),
	}
	a.fsMu.Lock()
	a.uploads[up.id] = up
	a.fsMu.Unlock()
	go a.gcUpload(up.id)
	a.reply(env.Seq, env.Sid, wire.MsgFSWriteOpenResult, &wire.FSWriteOpenResult{UploadID: up.id})
}

// fsWriteOpenDir 建临时目录 + io.Pipe, 启 goroutine 跑 UntarStream 消费管道读端。
// untar 结果通过 untarResult channel 在 commit 时取回。
func (a *Agent) fsWriteOpenDir(env *wire.Envelope, req *wire.FSWriteOpen, _ os.FileMode) {
	dir := filepath.Dir(req.Path)
	tmp, err := os.MkdirTemp(dir, ".dbg-uploaddir-*")
	if err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteOpenResult, &wire.FSWriteOpenResult{Err: err.Error()})
		return
	}
	pr, pw := io.Pipe()
	resultCh := make(chan *fsutil.UntarResult, 1)
	go func() {
		ures := fsutil.UntarStream(tmp, pr)
		resultCh <- ures
	}()
	up := &chunkedUpload{
		id: tmp, path: req.Path, isDir: true, wantSha: req.TotalSha256,
		pipeW: pw, untarResult: resultCh, partialDir: tmp, sha: sha256.New(),
		lastAct: time.Now(),
	}
	a.fsMu.Lock()
	a.uploads[up.id] = up
	a.fsMu.Unlock()
	go a.gcUpload(up.id)
	a.reply(env.Seq, env.Sid, wire.MsgFSWriteOpenResult, &wire.FSWriteOpenResult{UploadID: up.id})
}

func (a *Agent) fsWriteChunk(env *wire.Envelope, req *wire.FSWriteChunk) {
	a.fsMu.Lock()
	up, ok := a.uploads[env.Sid]
	a.fsMu.Unlock()
	if !ok {
		a.replyErr(env.Seq, env.Sid, "no_upload", "no such upload session")
		return
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	up.lastAct = time.Now()
	if sha256Hex(req.Data) != req.Sha256 {
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteChunkResult, &wire.FSWriteChunkResult{Index: req.Index, OK: false})
		return
	}
	var w io.Writer
	if up.isDir {
		// 写向 untar 管道 + 同步累计流 sha(供 commit 时端到端校验)。
		w = io.MultiWriter(up.pipeW, up.sha)
	} else {
		if _, err := up.f.Seek(req.Offset, io.SeekStart); err != nil {
			a.reply(env.Seq, env.Sid, wire.MsgFSWriteChunkResult, &wire.FSWriteChunkResult{Index: req.Index, Err: err.Error()})
			return
		}
		w = up.f
	}
	if _, err := w.Write(req.Data); err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteChunkResult, &wire.FSWriteChunkResult{Index: req.Index, Err: err.Error()})
		return
	}
	a.reply(env.Seq, env.Sid, wire.MsgFSWriteChunkResult, &wire.FSWriteChunkResult{Index: req.Index, OK: true})
}

func (a *Agent) fsWriteCommit(env *wire.Envelope, req *wire.FSWriteCommit) {
	a.fsMu.Lock()
	up, ok := a.uploads[env.Sid]
	if ok {
		delete(a.uploads, env.Sid)
	}
	a.fsMu.Unlock()
	if !ok {
		a.replyErr(env.Seq, env.Sid, "no_upload", "no such upload session")
		return
	}
	up.mu.Lock()
	defer up.mu.Unlock()

	if up.isDir {
		a.fsWriteCommitDir(env, up, req)
		return
	}
	a.fsWriteCommitFile(env, up, req)
}

func (a *Agent) fsWriteCommitFile(env *wire.Envelope, up *chunkedUpload, req *wire.FSWriteCommit) {
	res := &wire.FSWriteCommitResult{}
	passed := false
	wantSha := req.WantSha256
	if wantSha == "" {
		wantSha = up.wantSha
	}
	if _, err := up.f.Seek(0, io.SeekStart); err != nil {
		res.Err = err.Error()
	} else {
		h := sha256.New()
		size, err := io.Copy(h, up.f)
		if err != nil {
			res.Err = err.Error()
		} else {
			res.Size = size
			res.Sha256 = hex.EncodeToString(h.Sum(nil))
			if (req.WantSize > 0 && size != req.WantSize) || (wantSha != "" && res.Sha256 != wantSha) {
				res.Err = fmt.Sprintf("integrity check failed: size want=%d got=%d; sha256 want=%s got=%s",
					req.WantSize, size, wantSha, res.Sha256)
			} else {
				passed = true
			}
		}
	}
	_ = up.f.Close()
	if !passed {
		os.Remove(up.f.Name())
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteCommitResult, res)
		return
	}
	if err := os.Chmod(up.f.Name(), up.mode); err != nil {
		os.Remove(up.f.Name())
		res.Err = fmt.Sprintf("chmod: %v", err)
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteCommitResult, res)
		return
	}
	if err := os.Rename(up.f.Name(), up.path); err != nil {
		os.Remove(up.f.Name())
		res.Err = fmt.Sprintf("rename: %v", err)
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteCommitResult, res)
		return
	}
	a.reply(env.Seq, env.Sid, wire.MsgFSWriteCommitResult, res)
}

// fsWriteCommitDir: 关管道写端 -> 等 untar 完成 -> 比对流 sha/size -> rename 临时目录。
func (a *Agent) fsWriteCommitDir(env *wire.Envelope, up *chunkedUpload, req *wire.FSWriteCommit) {
	res := &wire.FSWriteCommitResult{}
	// 关闭管道写端 -> untar goroutine 读到 EOF, 完成 fsutil.UntarStream。
	// 若 chunk 阶段已因 pipe 写错误被关闭, 二次 Close 是幂等的(返回 ErrClosedPipe, 忽略)。
	_ = up.pipeW.Close()

	var ures *fsutil.UntarResult
	select {
	case ures = <-up.untarResult:
	case <-time.After(30 * time.Second):
		res.Err = "untar timed out"
		cleanupPartialDir(up.partialDir)
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteCommitResult, res)
		return
	}
	if ures == nil {
		res.Err = "untar result unavailable"
		cleanupPartialDir(up.partialDir)
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteCommitResult, res)
		return
	}

	// 端到端完整性: hub 侧累计的流 sha(WantSha256) vs agent 侧累计(up.sha) vs untar 消费(ures.Sha256)。
	agentSha := hex.EncodeToString(up.sha.Sum(nil))
	passed := ures.Err == nil &&
		(req.WantSize == 0 || req.WantSize == ures.Size) &&
		(req.WantSha256 == "" || req.WantSha256 == agentSha) &&
		agentSha == ures.Sha256
	if !passed {
		if ures.Err != nil {
			res.Err = ures.Err.Error()
		} else {
			res.Err = fmt.Sprintf("integrity check failed: size want=%d got=%d; sha hub=%s agent=%s untar=%s",
				req.WantSize, ures.Size, req.WantSha256, agentSha, ures.Sha256)
		}
		res.Entries = ures.Entries
		cleanupPartialDir(up.partialDir)
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteCommitResult, res)
		return
	}

	// 目标存在且非目录 -> 拒绝。
	if info, statErr := os.Stat(up.path); statErr == nil && !info.IsDir() {
		res.Err = fmt.Sprintf("target %q exists and is not a directory", up.path)
		res.Entries = ures.Entries
		cleanupPartialDir(up.partialDir)
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteCommitResult, res)
		return
	}
	if _, statErr := os.Stat(up.path); os.IsNotExist(statErr) {
		if err := os.Rename(up.partialDir, up.path); err != nil {
			res.Err = fmt.Sprintf("rename: %v", err)
			res.Entries = ures.Entries
			cleanupPartialDir(up.partialDir)
			a.reply(env.Seq, env.Sid, wire.MsgFSWriteCommitResult, res)
			return
		}
	} else {
		// 目标已存在目录: 把临时目录的 entries merge 进去。
		if err := mergeDir(up.partialDir, up.path); err != nil {
			res.Err = fmt.Sprintf("merge: %v", err)
			res.Entries = ures.Entries
			cleanupPartialDir(up.partialDir)
			a.reply(env.Seq, env.Sid, wire.MsgFSWriteCommitResult, res)
			return
		}
	}
	res.Size = ures.Size
	res.Sha256 = ures.Sha256
	res.Entries = ures.Entries
	a.reply(env.Seq, env.Sid, wire.MsgFSWriteCommitResult, res)
}

func cleanupPartialDir(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// mergeDir 把 src 的所有 entry 移进 dst(已存在的目录), 之后删空的 src。
func mergeDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())
		if err := os.Rename(from, to); err != nil {
			return err
		}
	}
	return os.RemoveAll(src)
}

// gcUpload 在空闲超时后删除未提交的上传及其临时文件/目录。
func (a *Agent) gcUpload(id string) {
	for {
		time.Sleep(fsSessionTimeout / 2)
		a.fsMu.Lock()
		up, ok := a.uploads[id]
		if !ok {
			a.fsMu.Unlock()
			return
		}
		up.mu.Lock()
		idle := time.Since(up.lastAct)
		isDir := up.isDir
		fname := ""
		pdir := up.partialDir
		if up.f != nil {
			fname = up.f.Name()
		}
		pipeW := up.pipeW
		up.mu.Unlock()
		if idle > fsSessionTimeout {
			delete(a.uploads, id)
			a.fsMu.Unlock()
			if isDir {
				if pipeW != nil {
					_ = pipeW.Close()
				}
				_ = os.RemoveAll(pdir)
			} else if fname != "" {
				_ = up.f.Close()
				_ = os.Remove(fname)
			}
			return
		}
		a.fsMu.Unlock()
	}
}

// --- download ---

func (a *Agent) fsReadOpen(env *wire.Envelope, req *wire.FSReadOpen) {
	chunkSz := req.ChunkSize
	if chunkSz <= 0 {
		chunkSz = defaultChunkSize
	}
	if req.IsDir {
		a.fsReadOpenDir(env, req, chunkSz)
		return
	}
	a.fsReadOpenFile(env, req, chunkSz)
}

func (a *Agent) fsReadOpenFile(env *wire.Envelope, req *wire.FSReadOpen, chunkSz int64) {
	fi, err := os.Stat(req.Path)
	if err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSReadOpenResult, &wire.FSReadOpenResult{Err: err.Error()})
		return
	}
	if fi.IsDir() {
		a.reply(env.Seq, env.Sid, wire.MsgFSReadOpenResult, &wire.FSReadOpenResult{Err: fmt.Sprintf("%q is a directory (is_dir=false)", req.Path)})
		return
	}
	f, err := os.Open(req.Path)
	if err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSReadOpenResult, &wire.FSReadOpenResult{Err: err.Error()})
		return
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		a.reply(env.Seq, env.Sid, wire.MsgFSReadOpenResult, &wire.FSReadOpenResult{Err: err.Error()})
		return
	}
	f.Close()
	dl := &chunkedDownload{
		id: fmt.Sprintf("d-%d", time.Now().UnixNano()), path: req.Path,
		total: fi.Size(), wantSha: hex.EncodeToString(h.Sum(nil)), chunkSz: chunkSz, lastAct: time.Now(),
	}
	a.fsMu.Lock()
	a.downloads[dl.id] = dl
	a.fsMu.Unlock()
	go a.gcDownload(dl.id)
	a.reply(env.Seq, env.Sid, wire.MsgFSReadOpenResult, &wire.FSReadOpenResult{
		DownloadID: dl.id, TotalSize: dl.total, TotalSha256: dl.wantSha, ChunkSize: dl.chunkSz,
	})
}

// fsReadOpenDir 启 goroutine 跑 fsutil.TarDir 写向管道写端, chunk 从管道读端取。
// tar 流的 total size/sha/entries 在流读尽后才能确定, 所以 open 阶段先回会话 + chunkSize,
// TotalSize/TotalSha256 留空(chunk 循环靠 EOF 收尾; hub 在 streamFromAgent 里读尽后比对)。
func (a *Agent) fsReadOpenDir(env *wire.Envelope, req *wire.FSReadOpen, chunkSz int64) {
	fi, err := os.Stat(req.Path)
	if err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSReadOpenResult, &wire.FSReadOpenResult{Err: err.Error()})
		return
	}
	if !fi.IsDir() {
		a.reply(env.Seq, env.Sid, wire.MsgFSReadOpenResult, &wire.FSReadOpenResult{Err: fmt.Sprintf("%q is not a directory (is_dir=true)", req.Path)})
		return
	}
	pr, pw := io.Pipe()
	r, res, err := fsutil.TarDir(req.Path)
	if err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSReadOpenResult, &wire.FSReadOpenResult{Err: err.Error()})
		return
	}
	// goroutine: 把 tar 流拷到 pipe 写端。调用方读尽 pr 即等价于 tar 流读尽。
	go func() {
		_, _ = io.Copy(pw, r)
		size, sha, n, terr := res.Result()
		if terr != nil {
			pw.CloseWithError(terr)
			return
		}
		// 把统计塞进 channel 供 commit/audit 用; 这里下载没有 commit, 暂存进 session。
		_ = size
		_ = sha
		_ = n
		pw.Close()
	}()
	dl := &chunkedDownload{
		id: fmt.Sprintf("d-%d", time.Now().UnixNano()), path: req.Path, isDir: true,
		chunkSz: chunkSz, pipeR: pr, lastAct: time.Now(),
	}
	a.fsMu.Lock()
	a.downloads[dl.id] = dl
	a.fsMu.Unlock()
	go a.gcDownload(dl.id)
	a.reply(env.Seq, env.Sid, wire.MsgFSReadOpenResult, &wire.FSReadOpenResult{
		DownloadID: dl.id, ChunkSize: dl.chunkSz, IsDir: true,
	})
}

func (a *Agent) fsReadChunk(env *wire.Envelope, req *wire.FSReadChunk) {
	a.fsMu.Lock()
	dl, ok := a.downloads[env.Sid]
	a.fsMu.Unlock()
	if !ok {
		a.replyErr(env.Seq, env.Sid, "no_download", "no such download session")
		return
	}
	dl.mu.Lock()
	dl.lastAct = time.Now()
	chunkSz, total, path, isDir, pipeR := dl.chunkSz, dl.total, dl.path, dl.isDir, dl.pipeR
	dl.mu.Unlock()

	if isDir {
		// 从 tar 管道读一块。io.ReadFull 处理 EOF/短读。
		buf := make([]byte, chunkSz)
		n, rerr := io.ReadFull(pipeR, buf)
		chunk := buf[:n]
		eof := rerr == io.EOF || rerr == io.ErrUnexpectedEOF
		if n == 0 && rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			a.reply(env.Seq, env.Sid, wire.MsgFSReadChunkResult, &wire.FSReadChunkResult{Index: req.Index, Err: rerr.Error()})
			return
		}
		a.reply(env.Seq, env.Sid, wire.MsgFSReadChunkResult, &wire.FSReadChunkResult{
			Index: req.Index, Data: chunk, Sha256: sha256Hex(chunk), EOF: eof,
		})
		return
	}

	// 单文件: 按 offset 从原文件读一块。
	f, err := os.Open(path)
	if err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSReadChunkResult, &wire.FSReadChunkResult{Index: req.Index, Err: err.Error()})
		return
	}
	defer f.Close()
	buf := make([]byte, chunkSz)
	n, rerr := f.ReadAt(buf, req.Offset)
	if rerr != nil && rerr != io.EOF {
		a.reply(env.Seq, env.Sid, wire.MsgFSReadChunkResult, &wire.FSReadChunkResult{Index: req.Index, Err: rerr.Error()})
		return
	}
	chunk := buf[:n]
	a.reply(env.Seq, env.Sid, wire.MsgFSReadChunkResult, &wire.FSReadChunkResult{
		Index: req.Index, Data: chunk, Sha256: sha256Hex(chunk),
		EOF: req.Offset+int64(n) >= total,
	})
}

func (a *Agent) gcDownload(id string) {
	for {
		time.Sleep(fsSessionTimeout / 2)
		a.fsMu.Lock()
		dl, ok := a.downloads[id]
		if !ok {
			a.fsMu.Unlock()
			return
		}
		dl.mu.Lock()
		idle := time.Since(dl.lastAct)
		pipeR := dl.pipeR
		dl.mu.Unlock()
		if idle > fsSessionTimeout {
			delete(a.downloads, id)
			a.fsMu.Unlock()
			if pipeR != nil {
				_ = pipeR.Close()
			}
			return
		}
		a.fsMu.Unlock()
	}
}
