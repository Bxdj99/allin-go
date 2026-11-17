package gateway

import "testing"

func TestTTFTBufferRecentOrder(t *testing.T) {
	b := newTTFTBuffer()
	for i := 1; i <= 5; i++ {
		b.Add(ttftSample{At: int64(i), TotalMs: int64(i * 100)})
	}
	got := b.Recent(3)
	if len(got) != 3 {
		t.Fatalf("期望 3 条，实际 %d", len(got))
	}
	// 应从旧到新，取最近 3 条即 3,4,5
	if got[0].TotalMs != 300 || got[2].TotalMs != 500 {
		t.Fatalf("顺序错误: %+v", got)
	}
}

func TestTTFTBufferWrap(t *testing.T) {
	b := newTTFTBuffer()
	// 塞满并溢出，验证环形覆盖后顺序仍正确
	for i := 1; i <= ttftCapacity+2; i++ {
		b.Add(ttftSample{At: int64(i), TotalMs: int64(i)})
	}
	all := b.Recent(0)
	if len(all) != ttftCapacity {
		t.Fatalf("环形缓冲应固定 %d 条，实际 %d", ttftCapacity, len(all))
	}
	// 最旧应为 3（前 2 条被覆盖），最新为 capacity+2
	if all[0].TotalMs != 3 {
		t.Fatalf("最旧样本应为 3，实际 %d", all[0].TotalMs)
	}
	if all[len(all)-1].TotalMs != int64(ttftCapacity+2) {
		t.Fatalf("最新样本错误: %d", all[len(all)-1].TotalMs)
	}
}

func TestTTFTSummary(t *testing.T) {
	b := newTTFTBuffer()
	for _, v := range []int64{100, 200, 300, 400, 5000} {
		b.Add(ttftSample{TotalMs: v, UpstreamMs: v - 10, OverheadMs: 10})
	}
	s := b.Summary(0)
	if s.Count != 5 {
		t.Fatalf("count=%d", s.Count)
	}
	if s.MaxMs != 5000 {
		t.Errorf("max=%d", s.MaxMs)
	}
	if s.SlowCount != 1 {
		t.Errorf("超 3s 应为 1 条，实际 %d", s.SlowCount)
	}
	if s.AvgOverMs != 10 {
		t.Errorf("平均开销应为 10，实际 %d", s.AvgOverMs)
	}
	if s.P90Ms <= 0 {
		t.Errorf("P90 异常: %d", s.P90Ms)
	}
}

func TestTTFTClear(t *testing.T) {
	b := newTTFTBuffer()
	b.Add(ttftSample{TotalMs: 100})
	b.Clear()
	if got := b.Recent(0); len(got) != 0 {
		t.Fatalf("清空后应为空，实际 %d 条", len(got))
	}
	if s := b.Summary(0); s.Count != 0 {
		t.Fatalf("清空后汇总应为 0，实际 %d", s.Count)
	}
}

func TestLogToggleWriter(t *testing.T) {
	// 关闭时 Write 仍返回成功但丢弃内容
	w := &toggleWriter{enabled: false, out: &countWriter{}}
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("关闭时应静默丢弃并报告成功: n=%d err=%v", n, err)
	}
	if cw := w.out.(*countWriter); cw.n != 0 {
		t.Fatal("关闭时不应写入底层")
	}
	w.set(true)
	_, _ = w.Write([]byte("hi"))
	if cw := w.out.(*countWriter); cw.n != 2 {
		t.Fatalf("开启后应写入底层，实际 %d", cw.n)
	}
}

type countWriter struct{ n int }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += len(p)
	return len(p), nil
}
