package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
)

// 标准库 log 的输出必须落成**恰好一行** zerolog 记录。
//
// 为什么值得一个测试：net/http 会在空闲 keep-alive 连接收到脏字节时直接
// log.Printf（transport.go 的 "Unsolicited response received on idle HTTP
// channel"），那行绕过 logger 硬写 os.Stderr，把进度块的相对定位基准顶偏，
// 之后整轮变成两块进度条。桥接本身一旦退化成多行输出，同样的错位会重演。
func TestLogBridgeKeepsStdLogOnOneLine(t *testing.T) {
	var buf bytes.Buffer
	l := zerolog.New(&buf)
	in := "Unsolicited response received on idle HTTP channel starting with \"\\r\\n\"\n"

	n, err := logBridge{l: &l}.Write([]byte(in))
	if err != nil {
		t.Fatalf("Write 返回错误: %v", err)
	}
	if n != len(in) {
		t.Fatalf("Write 应报告写入了全部 %d 字节，实际 %d", len(in), n)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("标准库 log 的输出没有进 zerolog 出口")
	}
	lines := bytes.Split(bytes.TrimRight([]byte(out), "\n"), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("应恰好输出一行，实际 %d 行:\n%s", len(lines), out)
	}

	var rec map[string]any
	if err := json.Unmarshal(lines[0], &rec); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, lines[0])
	}
	if rec["src"] != "stdlog" {
		t.Fatalf("应带 src=stdlog 便于识别来源，实际 %v", rec["src"])
	}
	if rec["level"] != "warn" {
		t.Fatalf("应以 warn 级别记录，实际 %v", rec["level"])
	}
	// 尾部换行必须被去掉，否则 zerolog 的 msg 里会夹一个裸换行。
	if msg, _ := rec["message"].(string); msg == "" || msg[len(msg)-1] == '\n' {
		t.Fatalf("msg 不应带尾部换行，实际 %q", rec["message"])
	}
}

// 空行不该产生日志记录：标准库偶尔会写纯换行，记成 warn 只是噪声。
func TestLogBridgeIgnoresBlankLine(t *testing.T) {
	var buf bytes.Buffer
	l := zerolog.New(&buf)

	n, err := logBridge{l: &l}.Write([]byte("\n"))
	if err != nil || n != 1 {
		t.Fatalf("Write(空行) 应返回 (1, nil)，实际 (%d, %v)", n, err)
	}
	if buf.Len() != 0 {
		t.Fatalf("空行不应产生日志，实际输出 %q", buf.String())
	}
}
