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
		// 以下语言里 puce/chip 位于型号之前,或用连字符连成 M4-chip,
		// 早期「要求 Apple 与 chip 相邻」的正则在这些站点识别率为 0。
		{"NL", "Refurbished 13‑inch MacBook Air Apple M4-chip met 10‑core CPU en 10‑core GPU - Zilver",
			Spec{"M4", 10, 10}},
		{"FR", "Mac mini reconditionné avec puce Apple M4, CPU 10 cœurs, GPU 10 cœurs et Gigabit Ethernet",
			Spec{"M4", 10, 10}},
		{"FR", "MacBook Air 15 pouces reconditionné avec puce Apple M4, CPU 10 cœurs et GPU 10 cœurs",
			Spec{"M4", 10, 10}},
		{"IT", "MacBook Air 13\" ricondizionato con chip Apple M2, CPU 8‑core e GPU 10‑core - Galassia",
			Spec{"M2", 8, 10}},
		{"ES", "iMac reacondicionado de 24 pulgadas con chip M4 de Apple, CPU de 8 núcleos y GPU de 8 núcleos",
			Spec{"M4", 8, 8}},
		{"KR", "리퍼비쉬 MacBook Pro 14 Apple M5 Pro 칩 모델(15코어 CPU 및 16코어 GPU) - 스페이스 블랙",
			Spec{"M5 Pro", 15, 16}},
		{"TW", "Mac mini Apple M4 晶片配備 10 核心 CPU 與 10 核心 GPU、乙太網路 (整修品)",
			Spec{"M4", 10, 10}},
		{"CH", "Refurbished 15\" MacBook Air mit Apple M4 Chip, 10‑Core CPU und 10‑Core GPU - Polarstern",
			Spec{"M4", 10, 10}},
	}

	for _, c := range cases {
		got := ParseSpec(c.title)
		if got != c.want {
			t.Errorf("%s: ParseSpec(%q)\n  got  %+v\n  want %+v", c.region, c.title, got, c.want)
		}
	}
}

// 解耦芯片指示词与型号后,必须确认不会把无关商品误判成有芯片。
func TestParseSpecNoFalsePositives(t *testing.T) {
	titles := []string{
		"翻新產品 Apple Studio Display,納米紋理玻璃,可調校斜度座架",
		"Refurbished Apple Studio Display - Nano-texture Glass",
		"Refurbished Apple Watch SE 3 GPS, 40mm Starlight Aluminum Case with M/L Starlight Sport Band",
		"翻新 Apple Pencil Pro",
		"Refurbished Magic Keyboard for iPad Pro 13-inch",
		"翻新 40 毫米星光色运动型表带 - S/M",
	}
	for _, title := range titles {
		if got := ParseSpec(title); got.Chip != "" {
			t.Errorf("不含芯片的商品被误判\n  标题: %s\n  识别为: %q", title, got.Chip)
		}
	}
}

// Apple 在同一个页面里混用普通空格与不间断空格分隔芯片型号和 Pro/Max
// (实测西/意/法站为 "M4 Pro",而同页的其它机型是 "M5 Pro")。
//
// 这里一律用 \u 转义构造样本:若写成字面字符,它们会在编辑器与补丁传输中
// 被悄悄替换成 ASCII,测试就会在归一化表失效时依然通过——这个 bug 真实发生过。
func TestParseSpecNormalizesSeparators(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  Spec
	}{
		{"U+00A0 分隔 tier(西班牙站)",
			"MacBook Neo reacondicionado con chip A18 Pro de Apple - Cítrico",
			Spec{"A18 Pro", 0, 0}},
		{"U+00A0 分隔 tier(意大利站)",
			"MacBook Pro 14\" ricondizionato con chip Apple M4 Max, CPU 16‑core e GPU 40‑core",
			Spec{"M4 Max", 16, 40}},
		{"U+2011 非断行连字符(德国站)",
			"Refurbished 13\" MacBook Air mit Apple M2 Chip, 8‑Core CPU und 10‑Core GPU",
			Spec{"M2", 8, 10}},
		{"U+2014 em dash(澳洲站)",
			"Refurbished Mac mini Apple M4 Chip with 10-Core CPU and 10-Core GPU — Silver",
			Spec{"M4", 10, 10}},
		{"U+202F 窄不间断空格",
			"Refurbished MacBook Pro Apple M5 Pro chip with 12-Core CPU",
			Spec{"M5 Pro", 12, 0}},
		{"U+3000 全角空格",
			"MacBook Pro Apple M5　Max 晶片",
			Spec{"M5 Max", 0, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseSpec(c.title); got != c.want {
				t.Errorf("ParseSpec(%q)\n  got  %+v\n  want %+v", c.title, got, c.want)
			}
		})
	}
}

// 归一化表必须真的把这些码位折成 ASCII。直接断言替换结果,
// 避免表项因字符被替换而退化成「空格换空格」这类无声失效。
func TestNormalizeTitleCoversCodepoints(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{"a‐b", "a-b"}, {"a‑b", "a-b"}, {"a‒b", "a-b"},
		{"a–b", "a-b"}, {"a—b", "a-b"}, {"a―b", "a-b"},
		{"a−b", "a-b"},
		{"a b", "a b"}, {"a b", "a b"},
		{"a b", "a b"}, {"a　b", "a b"},
	} {
		if got := NormalizeTitle(c.in); got != c.want {
			t.Errorf("NormalizeTitle(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
