// logtail.go:增量 tail 引擎日志,统计「hang 特征行」出现次数,作为判 hang 的第二确认信号。
//
// 为什么需要它(而不是让 sidecar 主动打一次 /health_generate):
//
//	sglang 的 /health 与 /health_generate 是【同一个 handler】(entrypoints/http_server.py:660-661),
//	且 SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION 默认 True(environ.py:329)—— 也就是说 openresty
//	每秒打的那个 /health 本身就是「发 1 个 token 等 detokenizer 回应」的真探活(实测耗时 1.0s,
//	正是 handler 里 await asyncio.sleep(1) 的粒度)。detokenizer 一卡,20s 后引擎自己就会打:
//	  Health check failed. Server couldn't get a response from detokenizer for last 20 seconds. ...
//	探活流量已经有人在驱动了,sidecar 再发一次请求 = 多一条请求 + 多一层要调的超时(而且必须 >
//	引擎侧 SGLANG_HEALTH_CHECK_TIMEOUT=20s,否则自己先超时、丢掉引擎 503 的明确信号)。
//	读日志是纯被动的:零请求、零超时,还能拿到「过去 window 秒内卡了几次」这种探测拿不到的历史。
package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 默认特征:sglang detokenizer 无响应。前缀匹配即可,后面的 "for last 20 seconds" 里的秒数
// 随 SGLANG_HEALTH_CHECK_TIMEOUT 变,不写进 pattern。
//
// pattern 是【一条正则】,不是列表:配了就【完全取代】这个默认值,不做叠加。要匹配多种特征行
// 用交替 `a|b|c`;想在自定义之外仍保留默认,把默认那段也写进交替分支里。
const defaultLogHangPattern = `Health check failed\. Server couldn't get a response from detokenizer`

// 单轮最多读的新增字节。超出说明日志爆量,直接跳到文件末尾(只关心「最近有没有」,不必读全)。
const maxTailBytes = 4 << 20

// logTailer:对单个日志文件(或 glob,取最新匹配)做增量 tail。非线程安全,只在 poll 循环里用。
type logTailer struct {
	pattern *regexp.Regexp
	glob    string

	cur  string      // 当前打开的文件路径
	f    *os.File    //
	off  int64       // 已读到的偏移
	info os.FileInfo // 用于 os.SameFile 判轮转(inode 变了 = 换文件)
	part string      // 上轮结尾的半行,拼到下轮开头
}

func newLogTailer(glob, pattern string) (*logTailer, error) {
	if pattern == "" {
		pattern = defaultLogHangPattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	return &logTailer{pattern: re, glob: glob}, nil
}

// newestMatch:glob 展开后取 mtime 最新的一个(k8s 的 /var/log/pods/.../<container>/*.log
// 会有 0.log、1.log… 轮转文件;裸机也可能直接给单个路径)。无匹配返回 ""。
func newestMatch(glob string) string {
	paths, err := filepath.Glob(glob)
	if err != nil || len(paths) == 0 {
		return ""
	}
	if len(paths) == 1 {
		return paths[0]
	}
	type ent struct {
		p string
		m int64
	}
	ents := make([]ent, 0, len(paths))
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		ents = append(ents, ent{p, st.ModTime().UnixNano()})
	}
	if len(ents) == 0 {
		return ""
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].m > ents[j].m })
	return ents[0].p
}

// open:打开(或重开)目标文件。fresh=true 表示首次打开,从【文件末尾】开始读 ——
// 不回溯历史,否则 sidecar 重启会把陈年旧告警当成刚发生的。
func (t *logTailer) open(path string, fresh bool) error {
	if t.f != nil {
		t.f.Close()
		t.f = nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	t.f, t.cur, t.info, t.part = f, path, st, ""
	if fresh {
		t.off = st.Size() // 只看启动之后的新行
	} else {
		t.off = 0 // 轮转出的新文件,从头读
	}
	_, err = t.f.Seek(t.off, io.SeekStart)
	return err
}

// poll:读出上次以来的新行,返回其中命中 pattern 的行数。
// 文件不存在/读不了返回 (0, err) —— 调用方据此决定「日志通道不可用」时怎么办(见 watcher)。
func (t *logTailer) poll() (int, error) {
	path := newestMatch(t.glob)
	if path == "" {
		return 0, os.ErrNotExist
	}
	if t.f == nil || path != t.cur {
		// 首次打开从末尾起;切到轮转出的新文件则从头读(新文件里的行都是新的)
		if err := t.open(path, t.f == nil); err != nil {
			return 0, err
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !os.SameFile(st, t.info) { // 同名不同 inode:被轮转/重建 → 从头读
		if err := t.open(path, false); err != nil {
			return 0, err
		}
		st, _ = os.Stat(path)
	}
	if st.Size() < t.off { // 被截断(copytruncate)→ 从头读
		t.off, t.part = 0, ""
		if _, err := t.f.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
	}
	if st.Size() == t.off {
		return 0, nil
	}
	if st.Size()-t.off > maxTailBytes { // 爆量:跳到末尾,只保留「最近」语义
		t.off, t.part = st.Size(), ""
		_, err := t.f.Seek(t.off, io.SeekStart)
		return 0, err
	}

	hits := 0
	r := bufio.NewReader(io.LimitReader(t.f, st.Size()-t.off))
	read := int64(0)
	for {
		line, err := r.ReadString('\n')
		read += int64(len(line))
		if err != nil { // 没读到换行 = 半行,留到下轮拼
			t.part += line
			break
		}
		full := t.part + line
		t.part = ""
		if t.pattern.MatchString(strings.TrimRight(full, "\r\n")) {
			hits++
		}
	}
	t.off += read
	return hits, nil
}

func (t *logTailer) close() {
	if t.f != nil {
		t.f.Close()
		t.f = nil
	}
}
