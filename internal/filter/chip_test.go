package filter

import "testing"

// 样本全部取自各地区线上页面的真实标题。
func TestParseSpec(t *testing.T) {
	cases := []struct {
		region string
		title  string
		want   Spec
	}{
		{"US", "Refurbished 14-inch MacBook Pro Apple M5 Pro chip with 12-Core CPU and 16-Core GPU",
			Spec{"M5 Pro", 12, 16}},
		{"US", "Refurbished Mac mini Apple M4 chip with 10-Core CPU and 10-Core GPU",
			Spec{"M4", 10, 10}},
		{"US", "Refurbished MacBook Neo Apple A18 Pro chip - Silver",
			Spec{"A18 Pro", 0, 0}},
		{"US", "Refurbished Apple Watch SE 3 GPS, 40mm Starlight Aluminum Case with S/M Starlight Sport Band",
			Spec{"", 0, 0}},
		// 德国站用 U+2011 非断行连字符,归一化后才能匹配
		{"DE", "Refurbished 13\" MacBook Air mit Apple M2 Chip, 8‑Core CPU und 10‑Core GPU - Space Grau",
			Spec{"M2", 8, 10}},
		{"CN", "翻新 Mac mini Apple M4 芯片 (配备 10 核中央处理器和 10 核图形处理器) 和千兆以太网端口",
			Spec{"M4", 10, 10}},
		{"CN", "翻新 13 英寸 MacBook Air Apple M2 芯片 (配备 8 核中央处理器和 8 核图形处理器) - 星光色",
			Spec{"M2", 8, 8}},
		{"CN", "翻新 MacBook Neo (Apple A18 Pro 芯片) - 银色",
			Spec{"A18 Pro", 0, 0}},
		// 香港用繁体「晶片」「核心 CPU」,与大陆文案完全不同
		{"HK", "翻新產品 14 吋 MacBook Pro Apple M5 晶片 (配備 10 核心 CPU 及 10 核心 GPU) - 太空黑",
			Spec{"M5", 10, 10}},
		{"HK", "翻新產品 24 吋 iMac Apple M4 晶片配備 8 核心 CPU 及 8 核心 GPU - 銀色",
			Spec{"M4", 8, 8}},
		{"HK", "翻新產品 Apple Studio Display,納米紋理玻璃,可調校斜度座架",
			Spec{"", 0, 0}},
		// 日本站把核心数写在芯片名之前,且 "Apple M4チップ" 无空格
		{"JP", "Mac mini [整備済製品] 10コアCPUと10コアGPUを搭載したApple M4チップ、10Gb Ethernet",
			Spec{"M4", 10, 10}},
		{"JP", "14インチMacBook Pro [整備済製品] 10コアCPUと10コアGPUを搭載したApple M5チップ - スペースブラック",
			Spec{"M5", 10, 10}},
		{"JP", "MacBook Neo [整備済製品] Apple A18 Pro チップ - シルバー",
			Spec{"A18 Pro", 0, 0}},
	}

	for _, c := range cases {
		got := ParseSpec(c.title)
		if got != c.want {
			t.Errorf("%s: ParseSpec(%q)\n  got  %+v\n  want %+v", c.region, c.title, got, c.want)
		}
	}
}
