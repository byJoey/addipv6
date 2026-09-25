package cfapi

import "testing"

func TestExpandNamesRoundRobin(t *testing.T) {
	// 不带占位符：三个地址都落到同一个名字，做 DNS 轮询。
	got, err := ExpandNames("v6", "example.com", 3)
	if err != nil {
		t.Fatalf("意外报错: %v", err)
	}
	for _, g := range got {
		if g != "v6.example.com" {
			t.Fatalf("想要 v6.example.com，拿到 %s", g)
		}
	}
}

func TestExpandNamesIndexed(t *testing.T) {
	got, err := ExpandNames("node-{n:2}", "example.com", 3)
	if err != nil {
		t.Fatalf("意外报错: %v", err)
	}
	want := []string{"node-01.example.com", "node-02.example.com", "node-03.example.com"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个想要 %s，拿到 %s", i, want[i], got[i])
		}
	}
}

func TestExpandNamesZeroBased(t *testing.T) {
	got, _ := ExpandNames("a{i}", "example.com", 2)
	if got[0] != "a0.example.com" || got[1] != "a1.example.com" {
		t.Fatalf("{i} 应该从 0 开始，拿到 %v", got)
	}
}

func TestQualify(t *testing.T) {
	cases := map[string]string{
		"":                     "example.com",
		"@":                    "example.com",
		"v6":                   "v6.example.com",
		"v6.example.com":       "v6.example.com",
		"A.B":                  "a.b.example.com",
		"example.com":          "example.com",
		"deep.sub.example.com": "deep.sub.example.com",
	}
	for in, want := range cases {
		got, err := Qualify(in, "example.com")
		if err != nil {
			t.Fatalf("%q 报错: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q 想要 %s，拿到 %s", in, want, got)
		}
	}
}

func TestQualifyRejectsGarbage(t *testing.T) {
	for _, in := range []string{"bad_name!", "a..b", "-lead", "trail-"} {
		if _, err := Qualify(in, "example.com"); err == nil {
			t.Fatalf("%q 应该被拒绝", in)
		}
	}
}

func TestHasPlaceholder(t *testing.T) {
	if !HasPlaceholder("x{n}") || !HasPlaceholder("x{i:3}") {
		t.Fatal("应该识别出占位符")
	}
	if HasPlaceholder("plain") {
		t.Fatal("不该识别出占位符")
	}
}
