package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// NeedsSeed 报告档案是否还需要用状态库的现有商品打底。
// 空文件同样算:创建文件之后、写入之前被 kill 会留下它,不认的话这次打底就永远补不上了。
func NeedsSeed(path string) (bool, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("检查历史档案 %s: %w", path, err)
	}
	return fi.Size() == 0, nil
}

// Append 把一批记录追加到档案末尾。
//
// 用追加而不是像状态文件那样整体重写:档案只增不减,几个月后有几十 MB,
// 每轮重写一遍既慢又让断电时丢失的范围从「最后一批」变成「整个文件」。
// 一批记录合成一次 Write 再 Sync,把写到一半的窗口压到最小。
func Append(path string, recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建历史档案目录: %w", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// 商品链接里的 & 不该被转义成 &,那会让人用 grep 查档案时对不上。
	enc.SetEscapeHTML(false)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("序列化历史记录 %s: %w", r.Key(), err)
		}
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开历史档案 %s: %w", path, err)
	}
	defer f.Close()

	// 上次写到一半断电会留下没有换行的残行。直接接着写,本批第一条记录会与残行
	// 粘成同一行一起解析失败——坏一行变成丢一条好数据。先补一个换行把它们隔开。
	torn, err := endsTorn(f)
	if err != nil {
		return fmt.Errorf("检查历史档案 %s 末尾: %w", path, err)
	}
	data := buf.Bytes()
	if torn {
		data = append([]byte{'\n'}, data...)
	}

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("写入历史档案 %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("同步历史档案 %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭历史档案 %s: %w", path, err)
	}
	return nil
}

// endsTorn 报告非空文件的最后一个字节是否不是换行。
func endsTorn(f *os.File) (bool, error) {
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if fi.Size() == 0 {
		return false, nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil {
		return false, err
	}
	return last[0] != '\n', nil
}

// maxLine 是单行上限。一行正常不到 1KB,放宽到 1MB 只为不让某条异常长的标题
// 触发 bufio.ErrTooLong 而中断整个文件的读取。
const maxLine = 1 << 20

// Read 读出档案中的全部记录。无法解析的行跳过并计入 bad,而不是让整个文件读不出来:
// 断电留下的残行只会坏一行,不该连累几个月的档案。
func Read(path string) (recs []Record, bad int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("打开历史档案: %w", err)
	}
	defer f.Close()
	return decode(f)
}

func decode(r io.Reader) (recs []Record, bad int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec Record
		if json.Unmarshal(line, &rec) != nil {
			bad++
			continue
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, bad, fmt.Errorf("读取历史档案: %w", err)
	}
	return recs, bad, nil
}
