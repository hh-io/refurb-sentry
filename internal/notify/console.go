package notify

import (
	"context"
	"fmt"
	"io"
)

// Console 把通知打印到标准输出,供 -dry-run 校验渲染结果而不真的发推送。
type Console struct {
	w io.Writer
	p phrases
}

func NewConsole(w io.Writer, lang Lang) *Console {
	return &Console{w: w, p: phrasesFor(lang)}
}

func (c *Console) Name() string { return "console(dry-run)" }

func (c *Console) Send(_ context.Context, m Message) error {
	_, err := fmt.Fprintf(c.w, c.p.consoleHeader, m.Title, m.Body, m.URL, m.Group)
	return err
}
