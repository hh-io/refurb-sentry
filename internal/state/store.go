package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Load 读取状态文件。文件不存在时返回一个未 bootstrap 的空状态,
// 首轮据此只建立基线而不发通知。
func Load(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取状态文件 %s: %w", path, err)
	}

	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("解析状态文件 %s(可手动删除该文件以重建基线): %w", path, err)
	}
	if s.Version != stateVersion {
		return nil, fmt.Errorf(
			"状态文件 %s 版本为 %d,当前程序期望 %d。请删除该文件:"+
				"下次启动会静默重建基线(不会产生误报通知),仅会丢失商品的首次发现时间",
			path, s.Version, stateVersion)
	}
	if s.Items == nil {
		s.Items = make(map[string]Entry)
	}
	if s.EmptyStreak == nil {
		s.EmptyStreak = make(map[string]int)
	}
	if s.Bootstrapped == nil {
		s.Bootstrapped = make(map[string]bool)
	}
	// 旧状态文件里没有 counters:日报计数从本次启动开始累计,不必升 stateVersion
	// 让所有存量用户重建基线——重建的代价(丢掉全部 first_seen)远大于少算一天计数。
	if s.Counters == nil {
		s.Counters = make(map[string]Counter)
	}
	return &s, nil
}

// Save 原子落盘:先写同目录下的临时文件再 rename,
// 保证进程被 kill 或断电时不会留下半截 JSON 让下次启动无法恢复。
func Save(path string, s *State) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建状态目录 %s: %w", dir, err)
	}

	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化状态: %w", err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时状态文件: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后此路径已不存在,失败时负责清理

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时状态文件: %w", err)
	}
	// rename 只保证元数据原子,数据仍需 fsync 才能扛住断电。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("同步临时状态文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时状态文件: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("替换状态文件 %s: %w", path, err)
	}
	return nil
}
